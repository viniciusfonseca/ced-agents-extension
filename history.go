package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type savedSession struct {
	SessionID string    `json:"sessionId"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type historyEntry struct {
	savedSession
	action string
	local  bool
}

type sessionHistory struct {
	loading bool
	err     error
	entries []historyEntry
}

func (m *sessions) refreshHistory(agent provider, force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.history[agent.ID]; current != nil && (current.loading || !force) {
		return
	}
	current := &sessionHistory{loading: true}
	if previous := m.history[agent.ID]; previous != nil {
		current.entries = previous.entries
	}
	if m.history == nil {
		m.history = make(map[string]*sessionHistory)
	}
	m.history[agent.ID] = current
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		entries, err := m.listHistory(agent)
		m.mu.Lock()
		defer m.mu.Unlock()
		current.loading, current.err = false, err
		if err == nil || len(entries) > 0 {
			current.entries = entries
		}
	}()
}

func (m *sessions) listHistory(agent provider) ([]historyEntry, error) {
	cmd, err := m.command(agent)
	if err != nil {
		return nil, err
	}
	p, err := startACP(cmd, m.root, func(p *acpPeer, msg rpcMessage) {
		if msg.ID != nil {
			p.reply(msg.ID, nil, &rpcError{Code: -32601, Message: "Unsupported ACP client method."})
		}
	})
	if err != nil {
		return nil, err
	}
	defer func() { p.stop(); <-p.done }()

	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()
	if _, err := initializeACP(ctx, p); err != nil {
		return nil, err
	}

	var entries []historyEntry
	params := map[string]string{}
	cursors := make(map[string]bool)
	seen := make(map[string]bool)
	local := make(map[string]bool)
	for {
		var result struct {
			Sessions   []savedSession
			NextCursor string
		}
		if err := p.exchange(ctx, "session/list", params, &result, nil); err != nil {
			var rpc *rpcError
			if errors.As(err, &rpc) && rpc.Code == -32601 {
				err = errors.New("This adapter does not expose session history. Update it to a version with ACP session listing.")
			}
			return entries, err
		}
		for _, item := range result.Sessions {
			if item.SessionID == "" || !filepath.IsAbs(item.Cwd) || seen[item.SessionID] {
				continue
			}
			seen[item.SessionID] = true
			related, known := local[item.Cwd]
			if !known {
				related = workspaceSession(m.root, item.Cwd)
				local[item.Cwd] = related
			}
			if strings.TrimSpace(item.Title) == "" {
				item.Title = "Untitled Session"
			}
			digest := sha256.Sum256([]byte(agent.ID + ":" + item.SessionID))
			entries = append(entries, historyEntry{
				savedSession: item, action: "history:" + hex.EncodeToString(digest[:]), local: related,
			})
		}
		if result.NextCursor == "" {
			return entries, nil
		}
		if cursors[result.NextCursor] {
			return entries, errors.New("The adapter repeated a session history page.")
		}
		cursors[result.NextCursor] = true
		params["cursor"] = result.NextCursor
	}
}

func workspaceSession(root, cwd string) bool {
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	relative, err := filepath.Rel(root, cwd)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

const sessionPageSize = 28

// The caller holds the sessions lock.
func (v *agentsView) historyRows() []historyEntry {
	var entries []historyEntry
	active := make(map[string]bool)
	for _, item := range v.sessions.items {
		if item.providerID != v.harness {
			continue
		}
		if item.remoteID != "" && item.Connected {
			active[item.remoteID] = true
		}
		if !item.Connected && item.savedID != "" {
			continue
		}
		entries = append(entries, historyEntry{
			savedSession: savedSession{Cwd: item.cwd, Title: item.Title, UpdatedAt: item.updatedAt},
			action:       "session:" + item.ID, local: item.local,
		})
	}
	if history := v.sessions.history[v.harness]; history != nil {
		for _, entry := range history.entries {
			if !active[entry.SessionID] {
				entries = append(entries, entry)
			}
		}
	}
	slices.SortStableFunc(entries, func(a, b historyEntry) int {
		if a.local != b.local {
			if a.local {
				return -1
			}
			return 1
		}
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return entries
}
