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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func agentCommand(agent provider) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("Agents requires an absolute HOME to find installed adapters.")
	}
	dirs := []string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	for _, dir := range dirs {
		path := filepath.Join(dir, agent.command)
		target, err := exec.LookPath(path)
		if err != nil {
			continue
		}
		searchPath := dirs
		if realPath, err := filepath.EvalSymlinks(target); err == nil {
			searchPath = append(append([]string{}, dirs...), filepath.Dir(realPath))
			// npm links resolve into lib/node_modules; Node is often beside that prefix's bin.
			if prefix, _, found := strings.Cut(realPath, "/lib/node_modules/"); found {
				searchPath = append(searchPath, filepath.Join(prefix, "bin"))
			}
		}
		cmd := exec.Command(target, agent.args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + strings.Join(searchPath, ":")}
		for _, key := range []string{"TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"} {
			if value, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		return cmd, nil
	}
	return nil, fmt.Errorf("Install %s and make %s available in ~/.local/bin or a system executable directory.", agent.Name, agent.command)
}

// ACP sends one JSON object per line. Replayed tool calls can include images.
const maxACPMessageBytes = 16 << 20

type acpPeer struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	done      chan struct{}
	readErr   error // Read after done is closed.
	writeMu   sync.Mutex
	mu        sync.Mutex
	next      int
	pending   map[string]chan rpcMessage
	onMessage func(*acpPeer, rpcMessage)
	stopOnce  sync.Once
}

func startACP(cmd *exec.Cmd, root string, onMessage func(*acpPeer, rpcMessage)) (*acpPeer, error) {
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	p := &acpPeer{cmd: cmd, stdin: stdin, stdout: stdout, pending: make(map[string]chan rpcMessage), done: make(chan struct{}), onMessage: onMessage}
	go p.read()
	return p, nil
}

type acpInitialization struct {
	ProtocolVersion   int
	AgentCapabilities struct {
		LoadSession bool
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
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
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
