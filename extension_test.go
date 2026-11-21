package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var mockContext = map[string]any{"sessionId": "ced-session", "workspaceId": "ced-workspace", "generation": 2}

func initialization(root string) map[string]any {
	limits := make(map[string]int)
	for _, key := range strings.Fields("maxHeaderBytes maxMessageBytes maxJsonDepth maxQueuedBytes maxPendingRequests maxDocumentBytes maxEditsPerTransaction maxSubscriptions maxCommandArgumentsBytes maxCommandResultBytes maxStateBytes maxPresentationBytes maxLogMessageBytes maxLogBytesPerSecond maxMessagesPer10Seconds initializeTimeoutMs activateTimeoutMs commandTimeoutMs serviceTimeoutMs deactivateTimeoutMs") {
		limits[key] = 1024
	}
	limits["maxMessageBytes"], limits["maxQueuedBytes"], limits["maxJsonDepth"] = 8<<20, 8<<20, 64
	limits["maxCommandResultBytes"] = 65536
	return map[string]any{
		"host":        map[string]any{"name": "CED", "version": "0.2.0", "tabProtocol": 1, "tabWidgets": 1},
		"extension":   map[string]string{"id": extensionID, "version": version},
		"apiVersions": []string{"1.0"}, "context": mockContext, "hostMethods": []string{}, "grantedCapabilities": []string{},
		"workspace": map[string]any{"uri": (&url.URL{Scheme: "file", Path: root}).String(), "trusted": true}, "limits": limits,
	}
}

func request(id int, method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("ced:%d", id), "method": method, "params": params}
}

func withContext(key string, value any) map[string]any {
	return map[string]any{"context": mockContext, key: value}
}

func openCommand(id int) map[string]any {
	return request(id, "commands/invoke", map[string]any{"context": mockContext, "commandId": commandID, "arguments": nil,
		"editor": map[string]any{"document": nil, "selection": nil, "selectionVersion": 0}})
}

type runningExtension struct {
	in      io.WriteCloser
	out     *bufio.Reader
	done    chan error
	process *os.Process
}

func launch(t *testing.T) *runningExtension {
	t.Helper()
	arch := strings.Replace(runtime.GOARCH, "amd64", "x86_64", 1)
	source := filepath.Join("bin", runtime.GOOS+"-"+arch, "agents")
	binary, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read package (run make package first): %v", err)
	}
	packageFolder, home := t.TempDir(), t.TempDir()
	executable := filepath.Join(packageFolder, "agents")
	if err := os.WriteFile(executable, binary, 0755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installFixtures(t, home, fixture)
	cmd := exec.Command(executable)
	cmd.Dir = packageFolder
	cmd.Env = []string{"HOME=" + home}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, childOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer childOutput.Close()
	t.Cleanup(func() { _ = out.Close() })
	cmd.Stdout = childOutput
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	r := &runningExtension{in: in, out: bufio.NewReader(out), done: done, process: cmd.Process}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
	})
	go func() { done <- cmd.Wait() }()
	return r
}

func installFixtures(t *testing.T, home, fixture string) {
	t.Helper()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		path := filepath.Join(bin, p.command)
		script := "#!/bin/sh\nexport CED_TEST_AGENT=" + p.ID + "\nexec " + shellQuote(fixture) + " -test.run '^TestAgentFixture$' -- \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func (r *runningExtension) receive(t *testing.T) rpcMessage {
	t.Helper()
	type response struct {
		body []byte
		err  error
	}
	ch := make(chan response, 1)
	go func() {
		body, err := readCED(r.out, 1024, 1<<20)
		ch <- response{body, err}
	}()
	select {
	case result := <-ch:
		var msg rpcMessage
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := json.Unmarshal(result.body, &msg); err != nil {
			t.Fatal(err)
		}
		return msg
	case <-time.After(7 * time.Second):
		t.Fatal("CED response timed out")
		return rpcMessage{}
	}
}

func (r *runningExtension) send(t *testing.T, msg any) {
	t.Helper()
	if err := writeCED(r.in, msg, 1024, 1<<20); err != nil {
		t.Fatal(err)
	}
}

func (r *runningExtension) expect(t *testing.T, id int, code int) rpcMessage {
	t.Helper()
	msg := r.receive(t)
	if string(msg.ID) != fmt.Sprintf("\"ced:%d\"", id) {
		t.Fatalf("wrong response ID: %+v", msg)
	}
	if code == 0 && (msg.Error != nil || msg.Result == nil) || code != 0 && (msg.Error == nil || msg.Error.Code != code) {
		t.Fatalf("wanted error %d, got %+v", code, msg)
	}
	return msg
}

func TestAgentFixture(t *testing.T) {
	agent := os.Getenv("CED_TEST_AGENT")
	if agent == "" {
		return
	}
	if agent == "devin" && os.Args[len(os.Args)-1] != "acp" {
		os.Exit(1)
	}
	s := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	write := func(value map[string]any) {
		value["jsonrpc"] = "2.0"
		if output.Encode(value) != nil {
			os.Exit(3)
		}
	}
	remote := agent + "-session"
	sendUpdate := func(update map[string]any) {
		write(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": remote, "update": update}})
	}
	model, effort := "standard", "low"
	config := func() []configOption {
		if agent == "devin" {
			return []configOption{}
		}
		return []configOption{
			{ID: "fixture-model", Category: "model", Type: "select", CurrentValue: model,
				Options: []configChoice{{Name: "Models", Options: []configChoice{
					{Value: "standard", Name: "Standard"}, {Value: "advanced", Name: "Advanced"},
				}}}},
			{ID: "fixture-effort", Category: "thought_level", Type: "select", CurrentValue: effort,
				Options: []configChoice{{Value: "low", Name: "Low"}, {Value: "high", Name: "High"}}},
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(2)
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		os.Exit(2)
	}
	outside := root + "-other"
	now := time.Now()
	var promptID json.RawMessage
	for s.Scan() {
		var msg rpcMessage
		if json.Unmarshal(s.Bytes(), &msg) != nil {
			os.Exit(2)
		}
		reply := func(result any) { write(map[string]any{"id": msg.ID, "result": result}) }
		switch msg.Method {
		case "initialize":
			capabilities := map[string]any{"loadSession": true}
			if agent == "codex" {
				capabilities["sessionCapabilities"] = map[string]any{"delete": struct{}{}}
			}
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": capabilities})
		case "session/delete":
			var params struct{ SessionID string }
			if json.Unmarshal(msg.Params, &params) != nil || agent != "codex" || params.SessionID != "codex-saved" {
				os.Exit(2)
			}
			reply(struct{}{})
		case "session/list":
			var params struct{ Cursor, Cwd string }
			if json.Unmarshal(msg.Params, &params) != nil || params.Cwd != "" {
				os.Exit(2)
			}
			if agent == "devin" {
				write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "No listing"}})
				continue
			}
			if params.Cursor == "" {
				items := []savedSession{{SessionID: agent + "-outside", Title: "Other " + agent, Cwd: outside, UpdatedAt: now}}
				for i := range 28 {
					id := agent + "-old-" + strconv.Itoa(i)
					items = append(items, savedSession{SessionID: id, Title: id, Cwd: outside, UpdatedAt: now.Add(-time.Duration(i+2) * time.Hour)})
				}
				reply(map[string]any{"sessions": items, "nextCursor": "fixture-next"})
			} else if params.Cursor == "fixture-next" {
				reply(map[string]any{"sessions": []savedSession{
					{SessionID: agent + "-outside", Title: "Duplicate", Cwd: outside, UpdatedAt: now},
					{SessionID: agent + "-saved", Title: "Saved " + agent, Cwd: root, UpdatedAt: now.Add(-time.Hour)},
					{SessionID: agent + "-nested", Title: "Nested " + agent, Cwd: filepath.Join(root, "nested"), UpdatedAt: now.Add(-30 * time.Minute)},
				}})
			} else {
				os.Exit(2)
			}
		case "session/load":
			var params struct {
				SessionID, Cwd string
				MCPServers     []any
			}
			if json.Unmarshal(msg.Params, &params) != nil || params.SessionID != agent+"-outside" ||
				params.Cwd != outside || params.MCPServers == nil {
				os.Exit(2)
			}
			remote = params.SessionID
			sendUpdate(map[string]any{"sessionUpdate": "user_message_chunk", "messageId": "user",
				"content": map[string]string{"type": "text", "text": "Old question"}})
			for _, part := range []string{"Inspect ", "the files"} {
				sendUpdate(map[string]any{"sessionUpdate": "agent_thought_chunk", "messageId": "thought",
					"content": map[string]string{"type": "text", "text": part}})
			}
			// Codex can replay tool content with images and escaped NULs.
			sendUpdate(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "image", "title": "Read files",
				"rawInput": map[string]string{"path": "."},
				"content": []any{map[string]any{"type": "content", "content": map[string]string{
					"type": "image", "mimeType": "image/png", "data": strings.Repeat("A", 3<<20),
				}}}})
			// Replayed tool output can contain a NUL separator, e.g. from git -z.
			sendUpdate(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "image",
				"status": "completed", "rawOutput": map[string]string{"stdout": "file-a\x00file-b\x00"}})
			for _, part := range []string{"Saved\x00", "answer"} {
				sendUpdate(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "answer",
					"content": map[string]string{"type": "text", "text": part}})
			}
			reply(map[string]any{"configOptions": config()})
		case "session/new":
			var params struct {
				Cwd        string
				MCPServers []any
			}
			if json.Unmarshal(msg.Params, &params) != nil || (params.Cwd != root && params.Cwd != filepath.Join(root, "nested")) || params.MCPServers == nil {
				os.Exit(2)
			}
			reply(map[string]any{"sessionId": remote, "configOptions": config()})
		case "session/set_config_option":
			var params struct{ SessionID, ConfigID, Value string }
			if json.Unmarshal(msg.Params, &params) != nil || params.SessionID != remote {
				os.Exit(6)
			}
			switch params.ConfigID {
			case "fixture-model":
				model = params.Value
				effort = "low"
			case "fixture-effort":
				effort = params.Value
			default:
				os.Exit(6)
			}
			reply(map[string]any{"configOptions": config()})
		case "session/prompt":
			promptID = msg.ID
			if strings.Contains(string(msg.Params), "wait") {
				sendUpdate(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "live",
					"title": "Check files", "status": "in_progress"})
				sendUpdate(map[string]any{"sessionUpdate": "agent_thought_chunk",
					"content": map[string]string{"type": "text", "text": "Checking the results"}})
				continue
			}
			sendUpdate(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "Olá "}})
			if strings.Contains(string(msg.Params), "permission") {
				write(map[string]any{"id": "permission-1", "method": "session/request_permission", "params": map[string]any{
					"sessionId": remote, "toolCall": map[string]string{"title": "Fixture permission"},
					"options": []permissionOption{{ID: "allow", Name: "Allow once", Kind: "allow_once"}},
				}})
			} else if !strings.Contains(string(msg.Params), "wait") {
				sendUpdate(map[string]any{"sessionUpdate": "session_info_update", "title": "Fixture " + agent})
				if agent != "devin" {
					effort = "low"
					sendUpdate(map[string]any{"sessionUpdate": "config_option_update", "configOptions": config()})
				}
				sendUpdate(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "mundo 🙂"}})
				reply(map[string]string{"stopReason": "end_turn"})
			}
		case "session/cancel":
			sendUpdate(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "live", "status": "failed",
				"content": []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": "Cancelled"}}}})
			write(map[string]any{"id": promptID, "result": map[string]string{"stopReason": "cancelled"}})
		case "":
			if string(msg.ID) != "\"permission-1\"" || !strings.Contains(string(msg.Result), "\"optionId\":\"allow\"") {
				os.Exit(4)
			}
			write(map[string]any{"id": promptID, "result": map[string]string{"stopReason": "end_turn"}})
		default:
			os.Exit(5)
		}
	}
	os.Exit(0)
}

func tabResponse(t *testing.T, response rpcMessage) tabModel {
	t.Helper()
	var result struct{ Tab tabModel }
	if err := json.Unmarshal(response.Result, &result); err != nil || result.Tab.Actions == nil {
		t.Fatalf("Invalid tab result: %s (%v)", response.Result, err)
	}
	return result.Tab
}

func TestModelSelectorLargeCatalog(t *testing.T) {
	options := make([]configChoice, 110)
	for i := range options {
		name := "model-" + strconv.Itoa(i)
		options[i] = configChoice{Value: name, Name: name}
	}
	s := session{Connected: true, config: []configOption{{
		ID: "model", Category: "model", Type: "select",
		CurrentValue: options[len(options)-1].Value, Options: options,
	}}}
	selector := s.selector("model", "Model")
	if len(selector.Options) != len(options) || selector.Selected != "model-109" {
		t.Fatalf("Incomplete model catalog: %+v", selector)
	}
}

func hasMessage(model tabModel, text string) bool {
	if model.Chat != nil {
		for _, message := range model.Chat.Messages {
			if strings.Contains(message.Text, text) {
				return true
			}
		}
	}
	return false
}

type tabClient struct {
	extension *runningExtension
	id        int
}

func (c *tabClient) action(t *testing.T, action, value string, code int) tabModel {
	t.Helper()
	c.id++
	message := openCommand(c.id)
	message["params"].(map[string]any)["arguments"] = []tabEvent{{TabID: tabID, Action: action, Value: value}}
	c.extension.send(t, message)
	response := c.extension.expect(t, c.id, code)
	if code != 0 {
		return tabModel{}
	}
	return tabResponse(t, response)
}

func (c *tabClient) wait(t *testing.T, ready func(tabModel) bool) tabModel {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		model := c.action(t, "refresh", "", 0)
		if ready(model) {
			return model
		}
	}
	t.Fatal("Timed out waiting for Agents tab")
	return tabModel{}
}

func TestPackagedEndToEnd(t *testing.T) {
	r := launch(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.send(t, openCommand(1))
	r.expect(t, 1, -32007)
	incompatible := initialization(root)
	incompatible["apiVersions"] = []string{"0.9"}
	r.send(t, request(2, "ced/initialize", incompatible))
	r.expect(t, 2, -32008)
	r.send(t, request(3, "ced/initialize", initialization(root)))
	r.expect(t, 3, 0)
	r.send(t, request(4, "ced/initialize", initialization(root)))
	r.expect(t, 4, -32010)
	var frames bytes.Buffer
	if err := writeCED(&frames, request(5, "ced/activate", withContext("reason", map[string]string{"event": "onCommand:" + commandID})), 1024, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, b := range frames.Bytes() {
		if _, err := r.in.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	r.expect(t, 5, 0)
	wrong := openCommand(6)
	wrong["params"].(map[string]any)["arguments"] = []string{"unexpected"}
	r.send(t, wrong)
	r.expect(t, 6, -32602)
	r.send(t, openCommand(7))
	model := tabResponse(t, r.expect(t, 7, 0))
	if model.Heading != "Agents" || len(model.Selectors) != 1 ||
		model.Selectors[0].Selected != "codex" || len(model.Selectors[0].Options) != len(providers) {
		t.Fatalf("Wrong empty view: %+v", model)
	}
	tab := tabClient{r, 7}
	tab.action(t, "harness", "unknown", -32010)
	for _, agent := range providers {
		model = tab.action(t, "harness", agent.ID, 0)
		if model.Selectors[0].Selected != agent.ID {
			t.Fatalf("Sessions leaked between harnesses: %+v", model)
		}
		tab.action(t, "new:"+agent.ID, "", 0)
		model = tab.wait(t, func(m tabModel) bool { return m.Input != nil })
		if model.Selectors[0].Placement != "" {
			t.Fatalf("Unexpected composer selector on tab widgets v1")
		}
		prompt := model.Input.ID
		id := strings.TrimPrefix(prompt, "prompt:")
		if agent.ID == "devin" {
			if model.Selectors[0].Selected != "" || model.Selectors[1].Selected != "" {
				t.Fatalf("Unexpected configuration: %+v", model.Selectors)
			}
		} else {
			if model.Selectors[0].Selected != "standard" || model.Selectors[1].Selected != "low" {
				t.Fatalf("Wrong initial configuration: %+v", model.Selectors)
			}
			tab.action(t, model.Selectors[0].ID, "unknown", -32010)
			tab.action(t, model.Selectors[0].ID, "advanced", 0)
			model = tab.wait(t, func(m tabModel) bool { return !m.Refresh && m.Selectors[0].Selected == "advanced" })
			tab.action(t, model.Selectors[1].ID, "high", 0)
			tab.wait(t, func(m tabModel) bool { return !m.Refresh && m.Selectors[1].Selected == "high" })
		}
		tab.action(t, prompt, "Olá 🙂", 0)
		model = tab.wait(t, func(m tabModel) bool { return m.Input != nil && hasMessage(m, "Olá mundo 🙂") })
		if agent.ID != "devin" && model.Selectors[1].Selected != "low" {
			t.Fatalf("Configuration update was missed: %+v", model.Selectors)
		}
		if model.Heading != "Fixture "+agent.ID+" · "+agent.Name {
			t.Fatalf("Wrong session: %+v", model)
		}
		tab.action(t, prompt, "wait", 0)
		model = tab.wait(t, func(m tabModel) bool { return m.Refresh && m.Input == nil })
		if len(model.Actions) < 2 || model.Actions[1].ID != "cancel:"+id {
			t.Fatalf("Missing legacy Stop action: %+v", model.Actions)
		}
		tab.action(t, prompt, "parallel", -32010)
		tab.action(t, "cancel:"+id, "", 0)
		model = tab.wait(t, func(m tabModel) bool { return m.Input != nil })
		if hasMessage(model, "Check files") || hasMessage(model, "Checking the results") {
			t.Fatal("Host with tab widgets v1 received activity entries")
		}
		tab.action(t, prompt, "permission", 0)
		var permission string
		tab.wait(t, func(m tabModel) bool {
			for _, action := range m.Actions {
				if strings.HasPrefix(action.ID, "permission:") && strings.HasSuffix(action.ID, ":0") {
					permission = action.ID
					return true
				}
			}
			return false
		})
		tab.action(t, permission, "", 0)
		tab.wait(t, func(m tabModel) bool { return m.Input != nil })
		tab.action(t, "back", "", 0)
	}
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if model.Actions[0].Label != "Fixture devin" || !strings.Contains(model.Text, "does not expose session history") {
		t.Fatalf("Wrong session list: %+v", model)
	}
	tab.action(t, "session:1", "", -32010)
	model = tab.action(t, model.Actions[0].ID, "", 0)
	if !hasMessage(model, "Olá mundo 🙂") {
		t.Fatalf("Missing reopened transcript: %+v", model)
	}
	tab.id++
	r.send(t, request(tab.id, "ced/deactivate", withContext("reason", "shutdown")))
	r.expect(t, tab.id, 0)
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Extension did not exit")
	}
}

func TestMachineSessionHistory(t *testing.T) {
	r := launch(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	init := initialization(root)
	init["host"].(map[string]any)["tabWidgets"] = 7
	r.send(t, request(1, "ced/initialize", init))
	r.expect(t, 1, 0)
	r.send(t, request(2, "ced/activate", withContext("reason", map[string]string{"event": "onCommand:" + commandID})))
	r.expect(t, 2, 0)
	tab := tabClient{r, 2}
	tab.action(t, "open", "", 0)
	model := tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if len(model.Actions) != 31 || !strings.HasPrefix(model.Actions[0].Label, "Nested codex") ||
		model.Actions[1].Label != "Saved codex" || !strings.HasPrefix(model.Actions[2].Label, "Other codex") ||
		model.Actions[28].ID != "history-page:1" {
		t.Fatalf("Wrong first page: %+v", model.Actions)
	}
	other := model.Actions[2].ID
	model = tab.action(t, model.Actions[28].ID, "", 0)
	if len(model.Actions) != 6 || !strings.HasPrefix(model.Actions[0].Label, "codex-old-25") {
		t.Fatalf("Wrong second page: %+v", model.Actions)
	}
	tab.action(t, "harness", "claude", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if !strings.HasPrefix(model.Actions[0].Label, "Nested claude") {
		t.Fatalf("Wrong list after switching harness: %+v", model.Actions)
	}
	tab.action(t, other, "", -32010)
	tab.action(t, "harness", "codex", 0)
	tab.action(t, other, "", 0)
	model = tab.wait(t, func(m tabModel) bool { return m.Input != nil })
	messages := model.Chat.Messages
	if len(messages) != 4 || messages[0] != (chatMessage{Role: "user", Text: "Old question"}) ||
		messages[1].Role != "reasoning" || messages[1].ID == "" || messages[1].Status != "" || messages[1].Text != "Inspect the files" ||
		messages[2].Role != "tool" || messages[2].ID == "" || messages[2].Status != "done" || messages[2].Text != "Read files" ||
		messages[2].Input != `{"path":"."}` || !strings.Contains(messages[2].Output, `"stdout":"file-a\u0000file-b\u0000"`) ||
		messages[3] != (chatMessage{Role: "agent", Text: "Saved answer"}) || model.Selectors[0].Selected != "standard" ||
		model.Selectors[0].Placement != "composer" || model.Selectors[1].Placement != "composer" {
		t.Fatalf("Replayed conversation did not load: %+v", model.Chat)
	}
	prompt := model.Input.ID
	tab.action(t, model.Selectors[0].ID, "advanced", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh && m.Selectors[0].Selected == "advanced" })
	tab.action(t, model.Selectors[1].ID, "high", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh && m.Selectors[1].Selected == "high" })
	if model.Input == nil || model.Input.ID != prompt || model.Input.Disabled {
		t.Fatalf("Configuration change replaced the composer: %+v", model.Input)
	}
	tab.action(t, prompt, "wait", 0)
	model = tab.wait(t, func(m tabModel) bool { return hasMessage(m, "Checking the results") })
	if model.Text != "Working..." {
		t.Fatalf("Unexpected busy label: %q", model.Text)
	}
	messages = model.Chat.Messages
	tool, thought := messages[len(messages)-2], messages[len(messages)-1]
	if tool.Role != "tool" || tool.Status != "running" || thought.Role != "reasoning" || thought.Status != "running" {
		t.Fatalf("Missing running activity: %+v", messages)
	}
	if model.Input == nil || !model.Input.Disabled || model.Input.ID != prompt ||
		model.Selectors[0].Selected != "advanced" || model.Selectors[1].Selected != "high" {
		t.Fatalf("Busy chat lost its composer or selected configuration: %+v", model)
	}
	if model.Input.Stop != "cancel:"+strings.TrimPrefix(prompt, "prompt:") {
		t.Fatalf("Missing composer stop action: %+v", model.Input)
	}
	for _, action := range model.Actions {
		if action.ID == model.Input.Stop {
			t.Fatal("Composer stop action was also listed in the header")
		}
	}
	tab.action(t, model.Input.Stop, "", 0)
	model = tab.wait(t, func(m tabModel) bool { return m.Input != nil && !m.Input.Disabled })
	if model.Input.Stop != "" {
		t.Fatal("Composer kept stop action after cancellation")
	}
	messages = model.Chat.Messages
	updatedTool, updatedThought := messages[len(messages)-2], messages[len(messages)-1]
	if updatedTool.ID != tool.ID || updatedTool.Status != "failed" || updatedTool.Output != "Cancelled" ||
		updatedThought.ID != thought.ID || updatedThought.Status != "" {
		t.Fatalf("Activity was not updated in place: %+v", messages)
	}
	tab.action(t, prompt, "Follow-up", 0)
	model = tab.wait(t, func(m tabModel) bool { return m.Input != nil && !m.Input.Disabled && hasMessage(m, "Olá mundo 🙂") })
	last := model.Chat.Messages[len(model.Chat.Messages)-1]
	if last.Role != "agent" || last.ID == "" {
		t.Fatalf("Live reply has no typewriter ID: %+v", last)
	}
	refreshed := tab.action(t, "refresh", "", 0)
	if refreshed.Chat.Messages[len(refreshed.Chat.Messages)-1].ID != last.ID {
		t.Fatal("Live reply changed ID on refresh")
	}
	if refreshed.Chat.Back != "back" {
		t.Fatalf("Missing chat back action: %+v", refreshed.Chat)
	}
	for _, action := range refreshed.Actions {
		if action.ID == refreshed.Chat.Back {
			t.Fatal("Back action was also listed in the header")
		}
	}
	model = tab.action(t, refreshed.Chat.Back, "", 0)
	if model.Chat != nil || len(model.Actions) != 31 || !strings.HasPrefix(model.Actions[2].ID, "session:") {
		t.Fatalf("Saved session was duplicated or not given its active connection: %+v", model.Actions)
	}
}

func TestGroupedSessionActions(t *testing.T) {
	r := launch(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	init := initialization(root)
	init["host"].(map[string]any)["tabWidgets"] = 8
	r.send(t, request(1, "ced/initialize", init))
	r.expect(t, 1, 0)
	r.send(t, request(2, "ced/activate", withContext("reason", map[string]string{"event": "onCommand:" + commandID})))
	r.expect(t, 2, 0)
	tab := tabClient{r, 2}
	tab.action(t, "open", "", 0)
	model := tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if len(model.Groups) != 3 || model.Groups[0].Label != nested || model.Groups[1].Label != root ||
		model.Groups[2].Label != root+"-other" || len(model.Groups[2].Items) != 26 {
		t.Fatalf("Wrong directory groups: %+v", model.Groups)
	}
	remove := model.Groups[1].Items[0].Delete
	model = tab.action(t, remove, "", 0)
	if len(model.Actions) != 2 || model.Actions[0].ID != "confirm-delete" || !strings.Contains(model.Text, "Saved codex") {
		t.Fatalf("Expected confirmation naming the session: %+v", model)
	}
	model = tab.action(t, "cancel-delete", "", 0)
	if len(model.Groups[1].Items) != 1 {
		t.Fatal("Cancel removed the session")
	}
	tab.action(t, remove, "", 0)
	tab.action(t, "confirm-delete", "", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if len(model.Groups) != 3 || model.Groups[0].Label != root || len(model.Groups[0].Items) != 0 {
		t.Fatalf("Deleted session is still in the history: %+v", model)
	}
	tab.action(t, model.Groups[1].Create, "", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if model.Input == nil || !strings.HasSuffix(model.Heading, nested) {
		t.Fatalf("New session did not start in the selected directory: %+v", model)
	}
	tab.action(t, "back", "", 0)
	tab.action(t, "harness", "claude", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	tab.action(t, model.Groups[1].Items[0].Delete, "", 0)
	tab.action(t, "confirm-delete", "", 0)
	model = tab.wait(t, func(m tabModel) bool { return !m.Refresh })
	if !strings.Contains(model.Text, "does not support deleting") || len(model.Groups[1].Items) != 1 {
		t.Fatalf("Unsupported deletion should keep the session and report the adapter error: %+v", model)
	}
}

func TestACPReadFixture(t *testing.T) {
	mode := os.Getenv("CED_TEST_ACP_READ")
	if mode == "" {
		return
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(1)
	}
	switch mode {
	case "oversized":
		fmt.Fprintln(os.Stdout, strings.Repeat("a", maxACPMessageBytes+3))
	case "invalid":
		fmt.Fprintln(os.Stdout, "{invalid}")
	case "raw-nul":
		fmt.Fprintln(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"text\":\"a\x00b\"}}")
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestACPReadErrors(t *testing.T) {
	for mode, want := range map[string]string{
		"oversized": "larger than 16 MiB",
		"invalid":   "invalid JSON-RPC",
		"raw-nul":   "invalid JSON-RPC",
	} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run", "^TestACPReadFixture$")
			cmd.Env = append(os.Environ(), "CED_TEST_ACP_READ="+mode)
			p, err := startACP(cmd, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { p.stop(); <-p.done }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = initializeACP(ctx, p)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

func TestCEDFraming(t *testing.T) {
	var input bytes.Buffer
	for _, value := range []string{"é🙂", "other"} {
		if err := writeCED(&input, map[string]string{"text": value}, 1024, 100); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(&input)
	for _, value := range []string{"é🙂", "other"} {
		body, err := readCED(reader, 1024, 100)
		if err != nil || string(body) != fmt.Sprintf("{\"text\":%q}", value) {
			t.Fatalf("%q %v", body, err)
		}
	}
	for _, frame := range []string{"Content-Length: 0\r\n\r\n", "Content-Length: +2\r\n\r\n{}", "Content-Length: 99999999999\r\n\r\n", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", strings.Repeat("x", 1025) + "\r\n"} {
		if _, err := readCED(bufio.NewReader(strings.NewReader(frame)), 1024, 100); err == nil {
			t.Fatalf("Accepted malformed frame: %q", frame)
		}
	}
	for _, body := range []string{`{"a":1,"a":2}`, `{"a":"\u0000"}`, "{\"a\":\"\xff\"}", `{"a":1e9999}`, `{"a":}`, `{} {}`, strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)} {
		if validateJSON([]byte(body), 64, false) == nil {
			t.Fatalf("Accepted malformed JSON: %q", body)
		}
	}
}

func TestShutdownDuringPrompt(t *testing.T) {
	for _, mode := range []string{"EOF", "SIGTERM"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r := launch(t)
			r.send(t, request(1, "ced/initialize", initialization(root)))
			r.expect(t, 1, 0)
			r.send(t, request(2, "ced/activate", withContext("reason", map[string]string{"event": "onCommand:" + commandID})))
			r.expect(t, 2, 0)
			r.send(t, openCommand(3))
			tabResponse(t, r.expect(t, 3, 0))
			tab := tabClient{r, 3}
			tab.action(t, "new:codex", "", 0)
			model := tab.wait(t, func(m tabModel) bool { return m.Input != nil })
			tab.action(t, model.Input.ID, "wait", 0)
			tab.wait(t, func(m tabModel) bool { return m.Refresh && hasMessage(m, "wait") })
			if mode == "EOF" {
				err = r.in.Close()
			} else {
				err = r.process.Signal(syscall.SIGTERM)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-r.done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Extension did not exit")
			}
		})
	}
}
