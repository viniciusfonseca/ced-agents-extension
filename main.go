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
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	extensionID = "viniciusfonseca.agents"
	version     = "0.4.3"
	commandID   = extensionID + ".open"
	tabID       = extensionID + ".panel"
)

type cedContext struct {
	SessionID   string  `json:"sessionId"`
	WorkspaceID string  `json:"workspaceId"`
	Generation  *uint64 `json:"generation"`
}

func (a cedContext) valid() bool {
	return a.SessionID != "" && len(a.SessionID) <= 128 && a.WorkspaceID != "" &&
		len(a.WorkspaceID) <= 128 && a.Generation != nil && *a.Generation <= 9007199254740991
}

func (a cedContext) matches(b cedContext) bool {
	return b.valid() && a.SessionID == b.SessionID && a.WorkspaceID == b.WorkspaceID && *a.Generation == *b.Generation
}

type cedPeer struct {
	context                                 cedContext
	workspace                               string
	active                                  bool
	tabWidgets                              int
	maxHeader, maxBody, maxDepth, maxResult int
	view                                    *agentsView
	lifetime                                context.Context
}

func domain(code int, reason, message string) *rpcError {
	return &rpcError{Code: code, Message: message, Data: map[string]string{"reason": reason}}
}

func (p *cedPeer) initialize(raw json.RawMessage) (any, *rpcError) {
	if p.workspace != "" {
		return nil, domain(-32010, "Conflict", "Already initialized.")
	}
	var init struct {
		Host struct {
			Name, Version           string
			TabProtocol, TabWidgets int
		}
		Extension   struct{ ID, Version string }
		APIVersions []string
		Context     cedContext
		Workspace   struct {
			URI     string
			Trusted bool
		}
		GrantedCapabilities []string
		HostMethods         []string
		Limits              map[string]int
	}
	invalid := &rpcError{Code: -32602, Message: "Invalid initialization parameters."}
	if json.Unmarshal(raw, &init) != nil || init.Host.Name != "CED" || init.Host.Version == "" ||
		!init.Context.valid() || init.HostMethods == nil || init.GrantedCapabilities == nil {
		return nil, invalid
	}
	if init.Extension.ID != extensionID || init.Extension.Version != version {
		return nil, domain(-32010, "Conflict", "Extension identity mismatch.")
	}
	if !slices.Contains(init.APIVersions, "1.0") {
		return nil, domain(-32008, "UnsupportedCapability", "CED API 1.0 is required.")
	}
	if init.Host.TabProtocol != 1 || init.Host.TabWidgets < 1 {
		return nil, domain(-32008, "UnsupportedCapability", "A CED build with native tab widgets is required.")
	}
	if !init.Workspace.Trusted {
		return nil, domain(-32001, "PermissionDenied", "Agents requires a trusted workspace.")
	}
	root, err := url.Parse(init.Workspace.URI)
	if err != nil || root.Scheme != "file" || root.Host != "" || root.User != nil ||
		root.RawQuery != "" || root.Fragment != "" || !filepath.IsAbs(root.Path) || strings.ContainsRune(root.Path, 0) {
		return nil, invalid
	}
	for _, name := range strings.Fields("maxHeaderBytes maxMessageBytes maxJsonDepth maxQueuedBytes maxPendingRequests maxDocumentBytes maxEditsPerTransaction maxSubscriptions maxCommandArgumentsBytes maxCommandResultBytes maxStateBytes maxPresentationBytes maxLogMessageBytes maxLogBytesPerSecond maxMessagesPer10Seconds initializeTimeoutMs activateTimeoutMs commandTimeoutMs serviceTimeoutMs deactivateTimeoutMs") {
		if init.Limits[name] < 1 {
			return nil, invalid
		}
	}
	p.maxHeader = min(p.maxHeader, init.Limits["maxHeaderBytes"])
	p.maxBody = min(p.maxBody, init.Limits["maxMessageBytes"], init.Limits["maxQueuedBytes"])
	p.maxDepth = min(p.maxDepth, init.Limits["maxJsonDepth"])
	p.maxResult = init.Limits["maxCommandResultBytes"]
	p.context, p.workspace = init.Context, root.Path
	p.tabWidgets = init.Host.TabWidgets
	return map[string]any{
		"extension":  map[string]string{"id": extensionID, "version": version},
		"apiVersion": "1.0", "requiredHostMethods": []string{},
	}, nil
}

func (p *cedPeer) handle(msg rpcMessage) (any, *rpcError, bool) {
	if msg.Method == "ced/initialize" {
		result, failure := p.initialize(msg.Params)
		return result, failure, false
	}
	var params struct {
		Context   cedContext
		Reason    json.RawMessage
		CommandID string
		Arguments json.RawMessage
		Editor    json.RawMessage
	}
	if json.Unmarshal(msg.Params, &params) != nil {
		return nil, &rpcError{Code: -32602, Message: "Invalid command parameters."}, false
	}
	if p.workspace == "" {
		return nil, domain(-32007, "NotActive", "Extension is not initialized."), false
	}
	if !p.context.matches(params.Context) {
		return nil, domain(-32003, "InvalidContext", "Invalid extension context."), false
	}
	switch msg.Method {
	case "ced/activate":
		var reason struct{ Event string }
		if json.Unmarshal(params.Reason, &reason) != nil || reason.Event != "onCommand:"+commandID {
			return nil, &rpcError{Code: -32602, Message: "Unknown activation event."}, false
		}
		if p.active {
			return nil, domain(-32007, "NotActive", "Extension cannot be activated twice."), false
		}
		p.active = true
	case "ced/deactivate":
		var reason string
		if json.Unmarshal(params.Reason, &reason) != nil ||
			!slices.Contains([]string{"disable", "uninstall", "workspaceChanged", "shutdown", "restart"}, reason) {
			return nil, &rpcError{Code: -32602, Message: "Invalid deactivation reason."}, false
		}
		if p.view != nil {
			p.view.cancel()
		}
		return struct{}{}, nil, true
	case "commands/invoke":
		if !p.active {
			return nil, domain(-32007, "NotActive", "Extension is not active."), false
		}
		if params.CommandID != commandID {
			return nil, &rpcError{Code: -32601, Message: "Unknown command."}, false
		}
		if len(params.Editor) == 0 || params.Editor[0] != '{' {
			return nil, &rpcError{Code: -32602, Message: "Missing editor context."}, false
		}
		event := tabEvent{TabID: tabID, Action: "open"}
		if string(params.Arguments) != "null" {
			var events []tabEvent
			if json.Unmarshal(params.Arguments, &events) != nil || len(events) != 1 ||
				events[0].TabID != tabID || len(events[0].Action) > 128 || len(events[0].Value) > maxPrompt {
				return nil, &rpcError{Code: -32602, Message: "Invalid Agents tab event."}, false
			}
			event = events[0]
		}
		if p.view == nil {
			p.view = startView(p.lifetime, p.workspace, p.tabWidgets)
		}
		if err := p.view.handle(event); err != nil {
			return nil, domain(-32010, "Conflict", err.Error()), false
		}
		result := map[string]tabModel{"tab": p.view.model()}
		wire, err := json.Marshal(result)
		if err != nil || len(wire)+128 > p.maxResult {
			return nil, domain(-32005, "ResourceLimit", "Agents tab exceeds the CED command result limit."), false
		}
		return result, nil, false
	default:
		return nil, &rpcError{Code: -32601, Message: "Unknown method."}, false
	}
	return struct{}{}, nil, false
}

func cedRequestID(id json.RawMessage, last *uint64) bool {
	var value string
	if json.Unmarshal(id, &value) != nil || !strings.HasPrefix(value, "ced:") {
		return false
	}
	digits := strings.TrimPrefix(value, "ced:")
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || digits != strconv.FormatUint(n, 10) || n <= *last {
		return false
	}
	*last = n
	return true
}

func runCED(ctx context.Context, input io.Reader, output io.Writer) error {
	p := cedPeer{lifetime: ctx, maxHeader: 1024, maxBody: 16 << 20, maxDepth: 64}
	defer func() {
		if p.view != nil {
			p.view.close()
		}
	}()
	reader := bufio.NewReaderSize(input, 1024)
	var lastID uint64
	for {
		body, err := readCED(reader, p.maxHeader, p.maxBody)
		if err != nil {
			return err
		}
		var msg rpcMessage
		reply := map[string]any{"jsonrpc": "2.0", "id": nil}
		var result any
		var failure *rpcError
		var stop bool
		if validateJSON(body, p.maxDepth) != nil {
			failure = &rpcError{Code: -32700, Message: "Invalid CED JSON."}
		} else if json.Unmarshal(body, &msg) != nil || msg.Version != "2.0" {
			failure = &rpcError{Code: -32600, Message: "Invalid CED request."}
		} else {
			if len(msg.ID) == 0 {
				continue // ced/cancel and other notifications have no reply or outstanding work.
			}
			if strings.HasPrefix(string(msg.ID), "\"ext:") && msg.Method == "" {
				continue // This extension never makes a host service request.
			}
			if !cedRequestID(msg.ID, &lastID) || msg.Method == "" || msg.Result != nil || msg.Error != nil {
				failure = &rpcError{Code: -32600, Message: "Invalid CED request."}
			} else {
				reply["id"] = msg.ID
				if !bytes.HasPrefix(msg.Params, []byte("{")) {
					failure = &rpcError{Code: -32602, Message: "Expected CED parameters object."}
				} else {
					result, failure, stop = p.handle(msg)
				}
			}
		}
		if failure != nil {
			reply["error"] = failure
		} else {
			reply["result"] = result
		}
		if err := writeCED(output, reply, p.maxHeader, p.maxBody); err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	input, err := pollablePipe(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CED input:", err)
		return
	}
	defer input.Close()
	output, err := pollablePipe(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CED output:", err)
		return
	}
	defer output.Close()
	go func() {
		<-ctx.Done()
		_ = input.Close()
		_ = output.Close()
	}()
	if err := runCED(ctx, input, output); err != nil && ctx.Err() == nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "CED protocol:", err)
	}
}

func pollablePipe(original *os.File) (*os.File, error) {
	// CED passes blocking pipe descriptors. Adopt a nonblocking duplicate so
	// closing it on SIGTERM wakes a pending read or write through Go's poller.
	fd, err := syscall.Dup(int(original.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), original.Name()), nil
}
