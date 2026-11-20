package main

import (
	"context"
	"errors"
	"slices"
	"time"
)

type configChoice struct {
	Value   string         `json:"value,omitempty"`
	Name    string         `json:"name,omitempty"`
	Options []configChoice `json:"options,omitempty"`
}

type configOption struct {
	ID           string         `json:"id"`
	Category     string         `json:"category"`
	Type         string         `json:"type"`
	CurrentValue string         `json:"currentValue"`
	Options      []configChoice `json:"options"`
}

type configResponse struct {
	ConfigOptions []configOption
}

func (s *session) configOption(category string) *configOption {
	for i := range s.config {
		option := &s.config[i]
		if option.Type == "select" && option.Category == category && option.ID != "" {
			return option
		}
	}
	for i := range s.config {
		option := &s.config[i]
		if option.Type == "select" && option.Category == "" &&
			(option.ID == category || category == "thought_level" && (option.ID == "effort" || option.ID == "reasoning_effort")) {
			return option
		}
	}
	return nil
}

func (option *configOption) choices() []configChoice {
	var choices []configChoice
	for _, entry := range option.Options {
		if entry.Options != nil {
			choices = append(choices, entry.Options...)
		} else {
			choices = append(choices, entry)
		}
	}
	return choices
}

func (m *sessions) setConfig(s *session, category, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil || !s.Connected || s.Busy || s.Cancelled || s.configuring {
		return errors.New("This session is busy or disconnected.")
	}
	option := s.configOption(category)
	if option == nil || value == "" || !slices.ContainsFunc(option.choices(), func(choice configChoice) bool {
		return choice.Value == value
	}) {
		return errors.New("That selection is no longer available.")
	}
	if option.CurrentValue == value {
		return nil
	}

	p, id, configID := s.peer, s.remoteID, option.ID
	revision := s.configRevision
	s.configuring, s.Error = true, ""
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		defer cancel()
		var result configResponse
		err := p.exchange(ctx, "session/set_config_option", map[string]string{
			"sessionId": id, "configId": configID, "value": value,
		}, &result, nil)
		m.mu.Lock()
		defer m.mu.Unlock()
		s.configuring = false
		if err != nil {
			s.Error = boundedText(err.Error(), 1024)
		} else if s.configRevision == revision {
			if result.ConfigOptions == nil {
				s.Error = "The adapter did not confirm the configuration."
			} else {
				s.config = result.ConfigOptions
			}
		}
	}()
	return nil
}
