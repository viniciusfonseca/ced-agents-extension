package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
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
	action  string
	local   bool
	working bool
}

type sessionHistory struct {
	loading  bool
	deleting bool
	err      error
	entries  []historyEntry
}

func (m *sessions) refreshHistory(agent provider, force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.history[agent.ID]; current != nil && (current.loading || current.deleting || !force) {
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
	root := m.root
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		entries, err := m.listHistory(agent, root)
		m.mu.Lock()
		defer m.mu.Unlock()
		current.loading, current.err = false, err
		if err == nil || len(entries) > 0 {
			localizeHistory(m.root, entries)
			current.entries = entries
		}
	}()
}

func (m *sessions) historyPeer(ctx context.Context, agent provider, root string) (*acpPeer, acpInitialization, error) {
	cmd, err := m.command(agent)
	if err != nil {
		return nil, acpInitialization{}, err
	}
	p, err := startACP(cmd, root, func(p *acpPeer, msg rpcMessage) {
		if msg.ID != nil {
			p.reply(msg.ID, nil, &rpcError{Code: -32601, Message: "Unsupported ACP client method."})
		}
	})
	if err != nil {
		return nil, acpInitialization{}, err
	}
	init, err := initializeACP(ctx, p)
	if err != nil {
		p.stop()
		<-p.done
		return nil, init, err
	}
	return p, init, nil
}

func (m *sessions) listHistory(agent provider, root string) ([]historyEntry, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()
	p, _, err := m.historyPeer(ctx, agent, root)
	if err != nil {
		return nil, err
	}
	defer func() { p.stop(); <-p.done }()

	var entries []historyEntry
	params := map[string]string{}
	cursors := make(map[string]bool)
	seen := make(map[string]bool)
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
			item.Cwd = filepath.Clean(item.Cwd)
			if strings.TrimSpace(item.Title) == "" {
				item.Title = "Untitled Session"
			}
			digest := sha256.Sum256([]byte(agent.ID + ":" + item.SessionID))
			entries = append(entries, historyEntry{
				savedSession: item, action: "history:" + hex.EncodeToString(digest[:]),
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

func localizeHistory(root string, entries []historyEntry) {
	local := make(map[string]bool)
	for i := range entries {
		cwd := entries[i].Cwd
		if _, known := local[cwd]; !known {
			local[cwd] = workspaceSession(root, cwd)
		}
		entries[i].local = local[cwd]
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
			working: item.Busy && item.turnDone != nil,
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

func groupAction(cwd string) string {
	digest := sha256.Sum256([]byte(cwd))
	return "new-directory:" + hex.EncodeToString(digest[:])
}

// Keep directories together on consecutive pages while retaining the order of
// their first appearance and the existing order of sessions within a directory.
func groupHistoryRows(rows []historyEntry) []historyEntry {
	var directories []string
	byDirectory := make(map[string][]historyEntry)
	for _, row := range rows {
		if byDirectory[row.Cwd] == nil {
			directories = append(directories, row.Cwd)
		}
		byDirectory[row.Cwd] = append(byDirectory[row.Cwd], row)
	}
	grouped := make([]historyEntry, 0, len(rows))
	for _, cwd := range directories {
		grouped = append(grouped, byDirectory[cwd]...)
	}
	return grouped
}

func sessionListLabel(text string, limit int) string {
	text = displayText(text, false, limit)
	// Bound JSON escaping so a page with 28 different directories fits the host limit.
	if wire, _ := json.Marshal(text); len(wire) > 900 {
		return boundedText(text, 149) + "…"
	}
	return text
}

func sessionGroup(cwd string) tabSessionGroup {
	return tabSessionGroup{Label: sessionListLabel(directoryLabel(cwd), 1024), Create: groupAction(cwd), Items: []tabSessionItem{}}
}

func directoryLabel(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return cwd
	}
	relative, err := filepath.Rel(home, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return cwd
	}
	return filepath.Join("~", relative)
}

func sessionGroups(rows []historyEntry, spinners bool) []tabSessionGroup {
	var groups []tabSessionGroup
	for i, row := range rows {
		if i == 0 || rows[i-1].Cwd != row.Cwd {
			groups = append(groups, sessionGroup(row.Cwd))
		}
		group := &groups[len(groups)-1]
		group.Items = append(group.Items, tabSessionItem{
			ID: row.action, Label: sessionListLabel(row.Title, 256), Delete: "delete:" + row.action, Busy: spinners && row.working,
		})
	}
	return groups
}

func (m *sessions) deleteSession(agent provider, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	history := m.history[agent.ID]
	if history != nil && (history.loading || history.deleting) {
		return errors.New("Wait for session history to finish updating.")
	}
	command, arg, _ := strings.Cut(action, ":")
	var remote string
	var live *session
	if command == "session" {
		for _, item := range m.items {
			if item.providerID == agent.ID && item.ID == arg {
				live, remote = item, item.remoteID
				break
			}
		}
		if live == nil {
			return errors.New("Session unavailable.")
		}
	} else if command == "history" && history != nil {
		for _, entry := range history.entries {
			if entry.action == action {
				remote = entry.SessionID
				break
			}
		}
		if remote == "" {
			return errors.New("Session unavailable.")
		}
	} else {
		return errors.New("Session unavailable.")
	}
	matches := func(item *session) bool {
		return item == live || (remote != "" && item.providerID == agent.ID &&
			(item.remoteID == remote || item.savedID == remote))
	}
	for _, item := range m.items {
		if matches(item) && (item.Busy || item.Cancelled || item.configuring) {
			return errors.New("Wait for the session to finish working before deleting it.")
		}
	}
	removeLive := func(item *session) bool {
		if !matches(item) {
			return false
		}
		if item.peer != nil {
			item.peer.stop()
		}
		return true
	}
	if remote == "" {
		m.items = slices.DeleteFunc(m.items, removeLive)
		return nil
	}
	if history == nil {
		history = &sessionHistory{}
		if m.history == nil {
			m.history = make(map[string]*sessionHistory)
		}
		m.history[agent.ID] = history
	}
	history.deleting, history.err = true, nil
	root := m.root
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		err := m.deleteRemoteSession(agent, root, remote)
		m.mu.Lock()
		defer m.mu.Unlock()
		history.deleting, history.err = false, err
		if err == nil {
			m.items = slices.DeleteFunc(m.items, removeLive)
			history.entries = slices.DeleteFunc(history.entries, func(entry historyEntry) bool {
				return entry.SessionID == remote
			})
		}
	}()
	return nil
}

func (m *sessions) deleteRemoteSession(agent provider, root, id string) error {
	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()
	p, init, err := m.historyPeer(ctx, agent, root)
	if err != nil {
		return err
	}
	defer func() { p.stop(); <-p.done }()
	if init.AgentCapabilities.SessionCapabilities.Delete == nil {
		return errors.New("This ACP adapter does not support deleting saved sessions.")
	}
	var result struct{}
	return p.exchange(ctx, "session/delete", map[string]string{"sessionId": id}, &result, nil)
}
