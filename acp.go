package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

type provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	command string
	args    []string
}

var providers = []provider{
	{ID: "codex", Name: "Codex", command: "codex-acp"},
	{ID: "claude", Name: "Claude Code", command: "claude-agent-acp"},
	{ID: "devin", Name: "Devin", command: "devin", args: []string{"acp"}},
}

// ACP sends one JSON object per line. Replayed tool calls can include images.
const maxACPMessageBytes = 16 << 20

type acpPeer struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	done        chan struct{}
	stopped     chan struct{}
	readErr     error // Read after done is closed.
	writeMu     sync.Mutex
	mu          sync.Mutex
	next        int
	pending     map[string]chan rpcMessage
	onMessage   func(*acpPeer, rpcMessage)
	stopOnce    sync.Once
	stopAdapter func()
}

func startACP(cmd *exec.Cmd, root string, onMessage func(*acpPeer, rpcMessage)) (*acpPeer, error) {
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	stopAdapter, err := startAdapter(cmd)
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	p := &acpPeer{cmd: cmd, stopAdapter: stopAdapter, stdin: stdin, stdout: stdout, pending: make(map[string]chan rpcMessage), done: make(chan struct{}), stopped: make(chan struct{}), onMessage: onMessage}
	go p.read()
	return p, nil
}

type acpInitialization struct {
	ProtocolVersion   int
	AgentCapabilities struct {
		LoadSession         bool
		SessionCapabilities struct {
			Delete *struct{}
		}
	}
}

func initializeACP(ctx context.Context, p *acpPeer) (acpInitialization, error) {
	var init acpInitialization
	err := p.exchange(ctx, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "ced-agents", "title": "CED Agents", "version": version},
	}, &init, nil)
	if err == nil && init.ProtocolVersion != 1 {
		err = errors.New("The adapter does not support ACP 1.")
	}
	return init, err
}

func (p *acpPeer) stop() {
	p.stopOnce.Do(func() {
		close(p.stopped)
		p.stopAdapter()
		_ = p.stdin.Close()
		_ = p.stdout.Close()
	})
}

func (p *acpPeer) read() {
	defer func() {
		p.stop()
		_ = p.cmd.Wait()
		close(p.done)
	}()
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 4096), maxACPMessageBytes+2)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > maxACPMessageBytes {
			p.readErr = oversizedACPMessage()
			return
		}
		var msg rpcMessage
		// ACP tool output may contain escaped NULs. Text shown in CED goes
		// through displayText before it reaches the native widgets.
		if validateJSON(line, 64, true) != nil || json.Unmarshal(line, &msg) != nil || msg.Version != "2.0" {
			p.readErr = errors.New("The ACP adapter sent an invalid JSON-RPC message.")
			return
		}
		if msg.Method != "" {
			p.onMessage(p, msg)
			continue
		}
		p.mu.Lock()
		reply := p.pending[string(msg.ID)]
		delete(p.pending, string(msg.ID))
		p.mu.Unlock()
		if reply != nil {
			reply <- msg
		}
	}
	if err := scanner.Err(); errors.Is(err, bufio.ErrTooLong) {
		p.readErr = oversizedACPMessage()
	} else if err != nil && !errors.Is(err, os.ErrClosed) {
		p.readErr = errors.New("Could not read a message from the ACP adapter.")
	}
}

func oversizedACPMessage() error {
	return fmt.Errorf("The ACP adapter sent a message larger than %d MiB.", maxACPMessageBytes>>20)
}

func (p *acpPeer) send(msg any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return json.NewEncoder(p.stdin).Encode(msg)
}

func (p *acpPeer) exchange(ctx context.Context, method string, params any, result any, sent chan struct{}) error {
	stopOnTimeout := context.AfterFunc(ctx, p.stop)
	defer stopOnTimeout()
	reply := make(chan rpcMessage, 1)
	p.mu.Lock()
	p.next++
	id := strconv.Itoa(p.next)
	p.pending[id] = reply
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err := p.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params}); err != nil {
		p.stop()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("The adapter stopped accepting requests.")
	}
	if sent != nil {
		close(sent)
	}
	select {
	case msg := <-reply:
		if msg.Error != nil {
			if msg.Error.Code == -32000 {
				return errors.New("Authentication required. Sign in using the agent's CLI, then create a new session.")
			}
			return msg.Error
		}
		if err := json.Unmarshal(msg.Result, result); err != nil || string(msg.Result) == "null" {
			return errors.New("Invalid ACP response.")
		}
		return nil
	case <-ctx.Done():
		p.stop()
		return ctx.Err()
	case <-p.done:
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.readErr != nil {
			return p.readErr
		}
		return errors.New("The ACP adapter disconnected.")
	}
}

func (p *acpPeer) notify(method string, params any) error {
	return p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *acpPeer) reply(id json.RawMessage, result any, failure *rpcError) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if failure == nil {
		reply["result"] = result
	} else {
		reply["error"] = failure
	}
	_ = p.send(reply)
}
