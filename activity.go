package main

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

type sessionUpdate struct {
	configResponse
	SessionUpdate, Title, Name, MessageID, ToolCallID, Status string
	Content, RawInput, RawOutput                              json.RawMessage
}

func (s *session) activityID() string {
	s.nextActivity++
	return s.ID + ":" + strconv.FormatUint(s.nextActivity, 10)
}

func (s *session) endReasoning() {
	if last := len(s.Messages) - 1; last >= 0 && s.Messages[last].Role == "reasoning" {
		s.Messages[last].Status = ""
	}
}

func (s *session) updateTool(update sessionUpdate) {
	if update.ToolCallID == "" || len(update.ToolCallID) > 1024 {
		return
	}
	index := slices.IndexFunc(s.Messages, func(message chatMessage) bool {
		return message.toolCallID == update.ToolCallID
	})
	if index < 0 {
		if update.SessionUpdate != "tool_call" {
			return
		}
		title := update.Title
		if title == "" {
			title = update.Name
		}
		if title == "" {
			title = "Tool call"
		}
		s.addMessage(chatMessage{Role: "tool", Text: boundedText(title, 256),
			ID: s.activityID(), Status: "pending", toolCallID: update.ToolCallID})
		index = len(s.Messages) - 1
	}
	message := &s.Messages[index]
	if update.Title != "" {
		s.totalText -= len(message.Text)
		message.Text = boundedText(update.Title, 256)
		s.totalText += len(message.Text)
	}
	switch update.Status {
	case "pending", "failed":
		message.Status = update.Status
	case "in_progress":
		message.Status = "running"
	case "completed":
		message.Status = "done"
	}
	if len(update.RawInput) != 0 && string(update.RawInput) != "null" {
		message.Input = toolPayload(update.RawInput)
	}
	if len(update.RawOutput) != 0 && string(update.RawOutput) != "null" {
		message.Output = toolPayload(update.RawOutput)
	} else if len(update.Content) != 0 && string(update.Content) != "null" {
		var content []struct {
			Type    string
			Content struct{ Type, Text string }
		}
		if json.Unmarshal(update.Content, &content) == nil {
			var preview string
			for _, item := range content {
				if item.Type == "content" && item.Content.Type == "text" && item.Content.Text != "" {
					if preview != "" {
						preview += "\n"
					}
					preview = toolPreview(preview + item.Content.Text)
					if toolPreviewLimit-len(preview) < len("…")+1 {
						break
					}
				}
			}
			message.Output = preview
		}
	}
	s.trimMessages()
	s.Revision++
}

const toolPreviewLimit = 1024

func toolPayload(raw json.RawMessage) string {
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return toolPreview(value)
		}
	}
	return toolPreview(string(raw))
}

func toolPreview(value string) string {
	if len(value) > toolPreviewLimit {
		value = boundedText(value, toolPreviewLimit-len("…")) + "…"
	}
	return displayText(strings.TrimSpace(value), true, toolPreviewLimit)
}
