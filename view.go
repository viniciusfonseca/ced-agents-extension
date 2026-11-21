package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type tabEvent struct {
	TabID  string `json:"tabId"`
	Action string `json:"action"`
	Value  string `json:"value"`
}

type tabAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type tabInput struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder"`
	Disabled    bool   `json:"disabled,omitempty"`
	Stop        string `json:"stop,omitempty"`
}

type tabSelector struct {
	ID        string      `json:"id"`
	Label     string      `json:"label"`
	Selected  string      `json:"selected"`
	Options   []tabAction `json:"options"`
	Disabled  bool        `json:"disabled,omitempty"`
	Placement string      `json:"placement,omitempty"`
}

type tabChat struct {
	Author   string        `json:"author"`
	Status   string        `json:"status,omitempty"`
	Back     string        `json:"back,omitempty"`
	Messages []chatMessage `json:"messages"`
}

type tabSessionGroup struct {
	Label  string           `json:"label"`
	Create string           `json:"create"`
	Items  []tabSessionItem `json:"items"`
}

type tabSessionItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Delete string `json:"delete"`
}

type tabModel struct {
	Heading   string            `json:"heading"`
	Text      string            `json:"text"`
	Actions   []tabAction       `json:"actions"`
	Input     *tabInput         `json:"input,omitempty"`
	Selectors []tabSelector     `json:"selectors,omitempty"`
	Chat      *tabChat          `json:"chat,omitempty"`
	Refresh   bool              `json:"refresh,omitempty"`
	Groups    []tabSessionGroup `json:"groups,omitempty"`
}

type agentsView struct {
	sessions      sessions
	selected      string
	harness       string
	page          int
	tabWidgets    int
	cancel        context.CancelFunc
	confirmDelete string
}

func startView(parent context.Context, workspace string, tabWidgets int) *agentsView {
	ctx, cancel := context.WithCancel(parent)
	return &agentsView{sessions: sessions{root: workspace, ctx: ctx, command: agentCommand}, harness: providers[0].ID, tabWidgets: tabWidgets, cancel: cancel}
}

func (v *agentsView) close() {
	v.cancel()
	v.sessions.jobs.Wait()
}

func (v *agentsView) findSession(id string) *session {
	for _, item := range v.sessions.items {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (v *agentsView) handle(event tabEvent) error {
	command, arg, _ := strings.Cut(event.Action, ":")
	if v.confirmDelete != "" && command != "refresh" {
		target := v.confirmDelete
		v.confirmDelete = ""
		switch command {
		case "confirm-delete":
			return v.sessions.deleteSession(v.currentHarness(), target)
		case "cancel-delete":
			return nil
		}
	}
	switch command {
	case "open":
		v.sessions.refreshHistory(v.currentHarness(), false)
		return nil
	case "refresh":
		return nil
	case "reload-history":
		v.sessions.refreshHistory(v.currentHarness(), true)
		return nil
	case "history-page":
		page, err := strconv.Atoi(arg)
		if v.selected != "" || err != nil || page < 0 {
			return errors.New("Session page unavailable.")
		}
		v.page = page
		return nil
	case "back":
		v.selected = ""
		return nil
	case "harness":
		if v.selected == "" && slices.ContainsFunc(providers, func(agent provider) bool { return agent.ID == event.Value }) {
			v.harness, v.page = event.Value, 0
			v.sessions.refreshHistory(v.currentHarness(), false)
			return nil
		}
		return errors.New("Choose an available harness.")
	case "delete":
		v.sessions.mu.Lock()
		defer v.sessions.mu.Unlock()
		if v.selected == "" && v.tabWidgets >= 8 {
			for _, row := range v.historyRows() {
				if row.action == arg {
					v.confirmDelete = arg
					return nil
				}
			}
		}
		return errors.New("Session unavailable.")
	case "new", "new-directory":
		if v.selected != "" {
			return errors.New("Choose an available agent from the session list.")
		}
		cwd := v.sessions.root
		if command == "new-directory" {
			v.sessions.mu.Lock()
			if groupAction(cwd) != event.Action {
				cwd = ""
				for _, row := range v.historyRows() {
					if groupAction(row.Cwd) == event.Action {
						cwd = row.Cwd
						break
					}
				}
			}
			v.sessions.mu.Unlock()
		} else if arg != v.harness {
			cwd = ""
		}
		if cwd == "" {
			return errors.New("Session directory unavailable.")
		}
		id, err := v.sessions.create(v.currentHarness(), cwd, nil)
		if err == nil {
			v.selected = id
		}
		return err
	case "history":
		if v.selected == "" {
			v.sessions.mu.Lock()
			var saved *historyEntry
			if history := v.sessions.history[v.harness]; history != nil {
				for i := range history.entries {
					if history.entries[i].action == event.Action {
						entry := history.entries[i]
						saved = &entry
						break
					}
				}
			}
			v.sessions.mu.Unlock()
			if saved != nil {
				id, err := v.sessions.create(v.currentHarness(), saved.Cwd, saved)
				if err == nil {
					v.selected = id
				}
				return err
			}
		}
		return errors.New("Session unavailable.")
	case "session":
		v.sessions.mu.Lock()
		defer v.sessions.mu.Unlock()
		item := v.findSession(arg)
		if v.selected != "" || item == nil || item.providerID != v.harness {
			return errors.New("Session unavailable.")
		}
		v.selected = arg
		return nil
	}

	id, details, _ := strings.Cut(arg, ":")
	v.sessions.mu.Lock()
	s := v.findSession(id)
	v.sessions.mu.Unlock()
	if id != v.selected || s == nil {
		return errors.New("Session unavailable.")
	}

	switch command {
	case "config":
		return v.sessions.setConfig(s, details, event.Value)
	case "prompt":
		return v.sessions.prompt(s, event.Value)
	case "cancel":
		return v.sessions.cancel(s)
	case "permission":
		request, choice, ok := strings.Cut(details, ":")
		if !ok {
			break
		}
		if choice == "deny" {
			return v.sessions.choose(s, request, "")
		}
		index, err := strconv.Atoi(choice)
		if err != nil || index < 0 {
			break
		}
		v.sessions.mu.Lock()
		prompt := s.Permission
		option := ""
		if prompt != nil && prompt.ID == request && index < len(prompt.Options) {
			option = prompt.Options[index].ID
		}
		v.sessions.mu.Unlock()
		if option != "" {
			return v.sessions.choose(s, request, option)
		}
	}
	return errors.New("That action is no longer available.")
}

func (v *agentsView) currentHarness() provider {
	return providers[slices.IndexFunc(providers, func(agent provider) bool { return agent.ID == v.harness })]
}

func displayText(value string, multiline bool, limit int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.Map(func(r rune) rune {
		if multiline && (r == '\n' || r == '\t') {
			return r
		}
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, value)
	return boundedText(value, limit)
}

func (v *agentsView) model() tabModel {
	v.sessions.mu.Lock()
	defer v.sessions.mu.Unlock()
	model := tabModel{Heading: "Agents", Actions: []tabAction{}}
	add := func(id, label string) {
		model.Actions = append(model.Actions, tabAction{id, displayText(label, false, 256)})
	}
	s := v.findSession(v.selected)
	if s == nil {
		harness := tabSelector{ID: "harness", Label: "Harness", Selected: v.harness}
		for _, agent := range providers {
			harness.Options = append(harness.Options, tabAction{ID: agent.ID, Label: agent.Name})
		}
		if v.confirmDelete != "" {
			for _, row := range v.historyRows() {
				if row.action == v.confirmDelete {
					model.Text = "Delete session “" + displayText(row.Title, false, 256) + "”? The adapter will remove it from its session history."
					add("confirm-delete", "Delete Session")
					add("cancel-delete", "Cancel")
					return model
				}
			}
			v.confirmDelete = ""
		}
		if history := v.sessions.history[v.harness]; history != nil && history.deleting {
			model.Text, model.Refresh = "Deleting session…", true
			return model
		}
		model.Selectors = []tabSelector{harness}
		rows := v.historyRows()
		if v.tabWidgets >= 8 {
			rows = groupHistoryRows(rows)
		}
		lastPage := max(0, (len(rows)-1)/sessionPageSize)
		v.page = min(v.page, lastPage)
		start := v.page * sessionPageSize
		pageRows := rows[start:min(start+sessionPageSize, len(rows))]
		if v.tabWidgets >= 8 {
			model.Groups = sessionGroups(pageRows)
			if v.page == 0 && !slices.ContainsFunc(rows, func(row historyEntry) bool { return row.Cwd == v.sessions.root }) {
				model.Groups = append([]tabSessionGroup{sessionGroup(v.sessions.root)}, model.Groups...)
			}
		} else {
			for _, row := range pageRows {
				label := row.Title
				if row.Cwd != v.sessions.root {
					label = boundedText(label, 125) + " · " + row.Cwd
				}
				add(row.action, label)
			}
		}
		if v.page > 0 {
			add("history-page:"+strconv.Itoa(v.page-1), "Previous Page")
		}
		if v.page < lastPage {
			add("history-page:"+strconv.Itoa(v.page+1), "Next Page")
		}
		if v.tabWidgets < 8 {
			add("new:"+v.harness, "New Session")
		}
		if history := v.sessions.history[v.harness]; history != nil {
			model.Refresh = history.loading
			switch {
			case history.loading:
				model.Text = "Loading session history…"
			case history.err != nil:
				model.Text = displayText(history.err.Error(), true, 1024)
			case len(rows) == 0:
				model.Text = "No Sessions"
			}
			if !history.loading {
				add("reload-history", "Refresh Sessions")
			}
		}
		return model
	}
	title := s.Title + " · " + s.Agent
	if s.cwd != v.sessions.root {
		title += " · " + s.cwd
	}
	model.Heading = displayText(title, false, 256)
	model.Refresh = s.Busy || s.Cancelled || s.configuring
	model.Selectors = []tabSelector{s.selector("model", "Model"), s.selector("thought_level", "Effort")}
	if v.tabWidgets >= 2 {
		model.Selectors[0].Placement = "composer"
	}
	if v.tabWidgets >= 3 {
		model.Selectors[1].Placement = "composer"
	}
	model.Chat = &tabChat{
		Author: s.Agent, Messages: transcript(s.Messages, v.tabWidgets),
	}
	if v.tabWidgets >= 7 {
		model.Chat.Back = "back"
	} else {
		add("back", "Sessions")
	}
	canStop := s.Connected && s.Busy && !s.Cancelled && s.turnDone != nil
	if canStop && v.tabWidgets < 5 {
		add("cancel:"+s.ID, "Stop")
	}
	status := "running"
	switch {
	case s.Error != "":
		model.Text, status = s.Error, "failed"
	case s.Cancelled:
		model.Text = "Stopping…"
	case s.Permission != nil:
		model.Text, status = s.Permission.Title, "pending"
	case s.Busy && !s.Connected:
		model.Text = "Connecting…"
	case s.Busy:
		model.Text = "Working..."
	case s.configuring:
		model.Text = "Updating session…"
	}
	if model.Text != "" {
		model.Text = displayText(model.Text, true, 1024)
		model.Chat.Status = status
	}
	if permission := s.Permission; permission != nil {
		prefix := "permission:" + s.ID + ":" + permission.ID + ":"
		for i, option := range permission.Options {
			add(prefix+strconv.Itoa(i), option.Name)
		}
		add(prefix+"deny", "Cancel Permission Request")
	}
	if s.Connected && (v.tabWidgets >= 3 || !s.Busy && !s.Cancelled) {
		model.Input = &tabInput{ID: "prompt:" + s.ID, Label: "Send", Placeholder: "Message",
			Disabled: s.Busy || s.Cancelled || s.configuring}
		if canStop && v.tabWidgets >= 5 {
			model.Input.Stop = "cancel:" + s.ID
		}
	}
	return model
}

func transcript(messages []chatMessage, tabWidgets int) []chatMessage {
	const textLimit = 8500
	const wireLimit = 12 << 10
	const activityTextLimit = 3600
	const activityWireLimit = 5 << 10
	result := []chatMessage{}
	length, encoded, activityText, activityWire, activities := 0, 0, 0, 0, 0
	latestTurn := true
	for i := len(messages) - 1; i >= 0 && len(result) < 64; i-- {
		message := messages[i]
		if message.Role == "user" {
			latestTurn = false
		}
		if message.Role == "agent" && (tabWidgets < 6 || !latestTurn) {
			message.ID = ""
		}
		remainingText, remainingWire := textLimit-length, wireLimit-encoded
		activity := message.Role == "reasoning" || message.Role == "tool"
		if activity {
			if tabWidgets < 4 || activities >= 24 {
				continue
			}
			remainingText = min(remainingText, activityTextLimit-activityText)
			remainingWire = min(remainingWire, activityWireLimit-activityWire)
		}
		if remainingText < 1 || remainingWire < 1 {
			continue
		}
		if message.Role == "tool" {
			message.Text = displayText(message.Text, false, 256)
			if len(message.Text)+len(message.Input)+len(message.Output) > remainingText {
				message.Input, message.Output = "", ""
			}
		} else {
			message.Text = tailText(displayText(message.Text, true, 256<<10), remainingText)
		}
		wire, _ := json.Marshal(message)
		if len(wire)+1 > remainingWire {
			if message.Role == "tool" {
				message.Input, message.Output = "", ""
			} else {
				// Leave room for metadata and JSON's maximum escaping overhead.
				message.Text = tailText(message.Text, max(0, (remainingWire-96)/6))
			}
			wire, _ = json.Marshal(message)
		}
		size := len(message.Text) + len(message.Input) + len(message.Output)
		if size > remainingText || len(wire)+1 > remainingWire || message.Text == "" {
			continue
		}
		length += size
		encoded += len(wire) + 1
		if activity {
			activityText += size
			activityWire += len(wire) + 1
			activities++
		}
		result = append(result, message)
	}
	slices.Reverse(result)
	return result
}

func tailText(text string, limit int) string {
	const prefix = "…\n"
	if len(text) <= limit {
		return text
	}
	if limit < len(prefix) {
		return ""
	}
	start := len(text) - limit + len(prefix)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return prefix + text[start:]
}

func (s *session) selector(category, label string) tabSelector {
	selector := tabSelector{
		ID: "config:" + s.ID + ":" + category, Label: label, Options: []tabAction{},
		Disabled: !s.Connected || s.Busy || s.Cancelled || s.configuring,
	}
	option := s.configOption(category)
	if option == nil {
		return selector
	}
	choices := option.choices()
	// Reserve room for the confirmed value even when the catalog exceeds the wire budget.
	if i := slices.IndexFunc(choices, func(choice configChoice) bool { return choice.Value == option.CurrentValue }); i > 0 {
		current := choices[i]
		copy(choices[1:i+1], choices[:i])
		choices[0] = current
	}
	const maxChoices = 256
	const maxOptionBytes = 16 << 10
	size := 0
	seen := make(map[string]bool)
	for _, choice := range choices {
		value := choice.Value
		if value == "" || displayText(value, false, 128) != value || seen[value] {
			continue
		}
		entry := tabAction{ID: value, Label: displayText(choice.Name, false, 128)}
		if strings.TrimSpace(entry.Label) == "" {
			entry.Label = value
		}
		wire, _ := json.Marshal(entry)
		if size+len(wire)+1 > maxOptionBytes {
			continue
		}
		seen[value] = true
		size += len(wire) + 1
		selector.Options = append(selector.Options, entry)
		if value == option.CurrentValue {
			selector.Selected = value
		}
		if len(selector.Options) == maxChoices {
			break
		}
	}
	return selector
}
