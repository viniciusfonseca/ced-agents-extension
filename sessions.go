package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxPrompt = 8 << 10

type chatMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type permissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type permissionPrompt struct {
	ID      string             `json:"id"`
	Title   string             `json:"title"`
	Options []permissionOption `json:"options"`
}

type session struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Agent          string            `json:"agent"`
	Busy           bool              `json:"busy"`
	Connected      bool              `json:"connected"`
	Cancelled      bool              `json:"cancelled"`
	Error          string            `json:"error,omitempty"`
	Messages       []chatMessage     `json:"messages"`
	Permission     *permissionPrompt `json:"permission,omitempty"`
	Revision       uint64            `json:"revision"`
	providerID     string
	cwd            string
	local          bool
	savedID        string
	updatedAt      time.Time
	config         []configOption
	configRevision uint64
	configuring    bool
	remoteID       string
	peer           *acpPeer
	permissionRPC  json.RawMessage
	turnDone       chan struct{}
	turnSent       chan struct{}
	splitMessage   bool
	replayMessage  string
	totalText      int
}

func boundedText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	for !utf8.RuneStart(text[maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func (s *session) appendMessage(role, text string) {
	text = boundedText(text, 256<<10)
	if text == "" {
		return
	}
	last := len(s.Messages) - 1
	if last >= 0 && s.Messages[last].Role == role && !s.splitMessage {
		turn := s.Messages[last].Text
		addition := boundedText(text, (256<<10)-len(turn))
		s.Messages[last].Text += addition
		s.totalText += len(addition)
	} else {
		s.Messages = append(s.Messages, chatMessage{Role: role, Text: text})
		s.totalText += len(text)
	}
	s.splitMessage = false
	for len(s.Messages) > 1 && (s.totalText > 512<<10 || len(s.Messages) > 240) {
		s.totalText -= len(s.Messages[0].Text)
		s.Messages[0] = chatMessage{}
		s.Messages = s.Messages[1:]
	}
	s.Revision++
}

type sessions struct {
	root    string
	ctx     context.Context
	command func(provider) (*exec.Cmd, error)
	mu      sync.Mutex
	jobs    sync.WaitGroup
	nextID  uint64
	items   []*session
	history map[string]*sessionHistory
}

func (m *sessions) create(agent provider, saved *historyEntry) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return "", m.ctx.Err()
	}
	if saved != nil {
		for _, item := range m.items {
			if item.providerID == agent.ID && (item.remoteID == saved.SessionID || item.savedID == saved.SessionID) && (item.Busy || item.Connected) {
				return item.ID, nil
			}
		}
		// A failed restore never takes the place of the saved history entry.
		m.items = slices.DeleteFunc(m.items, func(item *session) bool {
			return item.providerID == agent.ID && item.savedID == saved.SessionID && item.peer == nil && !item.Busy
		})
	}
	if len(m.items) >= 12 {
		// Saved sessions can be reopened, so release one idle connection to make room.
		idle := slices.IndexFunc(m.items, func(item *session) bool {
			return item.savedID != "" && !item.Busy && !item.Cancelled && !item.configuring
		})
		if idle < 0 {
			return "", errors.New("This Agents window cannot open more sessions.")
		}
		if p := m.items[idle].peer; p != nil {
			p.stop()
		}
		m.items = slices.Delete(m.items, idle, idle+1)
	}
	m.nextID++
	s := &session{ID: strconv.FormatUint(m.nextID, 10), Title: "New session", Agent: agent.Name, providerID: agent.ID,
		cwd: m.root, local: true, updatedAt: time.Now(), Busy: true, Revision: 1}
	if saved != nil {
		s.Title, s.cwd, s.local = boundedText(saved.Title, 100), saved.Cwd, saved.local
		s.savedID, s.updatedAt = saved.SessionID, saved.UpdatedAt
		s.remoteID = saved.SessionID
	}
	m.items = append(m.items, s)
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		m.connect(s, agent)
	}()
	return s.ID, nil
}

func (m *sessions) connect(s *session, agent provider) {
	cmd, err := m.command(agent)
	if err != nil {
		m.finish(s, err)
		return
	}
	p, err := startACP(cmd, m.root, func(p *acpPeer, msg rpcMessage) { m.update(s, p, msg) })
	if err != nil {
		m.finish(s, errors.New("Could not launch "+agent.Name+". Check the adapter's installation."))
		return
	}
	m.mu.Lock()
	s.peer = p
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()
	init, err := initializeACP(ctx, p)
	if err == nil {
		var result struct {
			SessionID string
			configResponse
		}
		params := map[string]any{"cwd": s.cwd, "mcpServers": []any{}}
		if s.savedID == "" {
			err = p.exchange(ctx, "session/new", params, &result, nil)
			if err == nil && result.SessionID == "" {
				err = errors.New("The adapter returned an empty ACP session ID.")
			}
		} else if init.AgentCapabilities.LoadSession {
			params["sessionId"] = s.savedID
			err = p.exchange(ctx, "session/load", params, &result, nil)
			result.SessionID = s.savedID
		} else {
			err = errors.New("This adapter cannot reopen saved sessions. Update it to a version with ACP session loading.")
		}
		if err == nil {
			m.mu.Lock()
			s.remoteID = result.SessionID
			if result.ConfigOptions != nil {
				s.config = result.ConfigOptions
			}
			s.Connected = true
			m.mu.Unlock()
		}
	}
	m.finish(s, err)
	if err != nil {
		p.stop()
	}

	// Track the child until it exits, including after a successful handshake.
	select {
	case <-m.ctx.Done():
		p.stop()
		<-p.done
	case <-p.done:
	}
	m.mu.Lock()
	if s.peer == p {
		s.peer = nil
		s.Connected = false
		if s.Error == "" {
			if p.readErr != nil {
				s.Error = p.readErr.Error()
			} else {
				s.Error = "The ACP adapter disconnected."
			}
		}
		s.Revision++
	}
	m.mu.Unlock()
}

func (m *sessions) finish(s *session, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Busy, s.Permission = false, nil
	s.permissionRPC = nil
	if err != nil {
		s.Error = boundedText(err.Error(), 1024)
	}
	s.Revision++
}

func (m *sessions) prompt(s *session, text string) error {
	if strings.TrimSpace(text) == "" || len(text) > maxPrompt {
		return errors.New("Enter a message of up to 8 KiB.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil || s.Busy || s.Cancelled || s.configuring || !s.Connected {
		return errors.New("This session is busy or disconnected.")
	}
	p, id := s.peer, s.remoteID
	if s.Title == "New session" {
		title := strings.Join(strings.Fields(text), " ")
		s.Title = boundedText(title, 100)
	}
	s.Busy, s.Error = true, ""
	s.updatedAt = time.Now()
	s.splitMessage = true
	done, sent := make(chan struct{}), make(chan struct{})
	s.turnDone, s.turnSent = done, sent
	s.appendMessage("user", text)
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		defer close(done)
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
		defer cancel()
		var result struct{ StopReason string }
		err := p.exchange(ctx, "session/prompt", map[string]any{
			"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": text}},
		}, &result, sent)
		if err == nil && result.StopReason != "end_turn" && result.StopReason != "cancelled" {
			err = errors.New("Prompt stopped: " + result.StopReason)
		}
		m.finish(s, err)
	}()
	return nil
}

func (m *sessions) cancel(s *session) error {
	m.mu.Lock()
	if m.ctx.Err() != nil || s.Cancelled || !s.Busy || !s.Connected || s.turnDone == nil {
		m.mu.Unlock()
		return errors.New("There is no prompt to stop.")
	}
	p, id, done, sent := s.peer, s.remoteID, s.turnDone, s.turnSent
	s.Cancelled = true
	s.Revision++
	pendingRPC := s.permissionRPC
	s.Permission, s.permissionRPC = nil, nil
	m.jobs.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.jobs.Done()
		timer := time.AfterFunc(10*time.Second, func() {
			select {
			case <-done:
			default:
				p.stop()
			}
		})
		defer timer.Stop()
		select {
		case <-sent:
			if pendingRPC != nil {
				replyPermission(p, pendingRPC, "")
			}
			_ = p.notify("session/cancel", map[string]string{"sessionId": id})
		case <-m.ctx.Done():
		case <-done:
		}
		select {
		case <-done:
		case <-m.ctx.Done():
		}
		m.mu.Lock()
		s.Cancelled = false
		s.Revision++
		m.mu.Unlock()
	}()
	return nil
}

func replyPermission(p *acpPeer, id json.RawMessage, option string) {
	outcome := map[string]string{"outcome": "cancelled"}
	if option != "" {
		outcome = map[string]string{"outcome": "selected", "optionId": option}
	}
	p.reply(id, map[string]any{"outcome": outcome}, nil)
}

func (m *sessions) choose(s *session, id, option string) error {
	m.mu.Lock()
	if m.ctx.Err() != nil || s.peer == nil || s.Permission == nil || s.Permission.ID != id ||
		option != "" && !slices.ContainsFunc(s.Permission.Options, func(o permissionOption) bool { return o.ID == option }) {
		m.mu.Unlock()
		return errors.New("That permission request is no longer available.")
	}
	p, rpcID := s.peer, s.permissionRPC
	s.Permission, s.permissionRPC = nil, nil
	s.Revision++
	m.jobs.Add(1)
	m.mu.Unlock()
	go func() { defer m.jobs.Done(); replyPermission(p, rpcID, option) }()
	return nil
}

func (m *sessions) update(s *session, p *acpPeer, msg rpcMessage) {
	switch msg.Method {
	case "session/update":
		var params struct {
			SessionID string
			Update    struct {
				configResponse
				SessionUpdate, Title, MessageID string
				Content                         struct{ Type, Text string }
			}
		}
		if msg.ID != nil || json.Unmarshal(msg.Params, &params) != nil || params.SessionID == "" {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if s.remoteID != "" && s.remoteID != params.SessionID {
			return
		}
		update := params.Update
		switch update.SessionUpdate {
		case "agent_message_chunk", "user_message_chunk":
			replaying := s.savedID != "" && !s.Connected && s.Busy
			if s.Busy && s.remoteID != "" && update.Content.Type == "text" {
				if replaying && update.MessageID != s.replayMessage {
					s.splitMessage = true
					s.replayMessage = update.MessageID
				}
				if update.SessionUpdate == "agent_message_chunk" {
					s.appendMessage("agent", update.Content.Text)
				} else if replaying {
					s.appendMessage("user", update.Content.Text)
				}
			}
		case "session_info_update":
			if title := strings.TrimSpace(update.Title); title != "" {
				s.Title = boundedText(title, 100)
				s.Revision++
			}
		case "config_option_update":
			if s.remoteID == params.SessionID && update.ConfigOptions != nil {
				s.config = update.ConfigOptions
				s.configRevision++
			}
		case "tool_call":
			s.splitMessage = true
		}
	case "session/request_permission":
		if msg.ID == nil {
			return
		}
		var params struct {
			SessionID string
			ToolCall  struct{ Title string }
			Options   []permissionOption
		}
		err := json.Unmarshal(msg.Params, &params)
		m.mu.Lock()
		if err == nil && params.SessionID != "" && params.SessionID == s.remoteID && s.Connected && s.Busy && !s.Cancelled &&
			s.Permission == nil && len(params.Options) <= 12 {
			options := make([]permissionOption, 0, len(params.Options))
			for _, option := range params.Options {
				if option.ID != "" && len(option.ID) <= 128 && (option.Kind == "allow_once" || option.Kind == "reject_once") {
					option.Name = boundedText(option.Name, 128)
					options = append(options, option)
				}
			}
			s.Revision++
			s.Permission = &permissionPrompt{ID: strconv.FormatUint(s.Revision, 10), Title: boundedText(params.ToolCall.Title, 256), Options: options}
			s.permissionRPC = msg.ID
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		replyPermission(p, msg.ID, "")
	default:
		if msg.ID != nil {
			p.reply(msg.ID, nil, &rpcError{Code: -32601, Message: "Unsupported ACP client method."})
		}
	}
}
