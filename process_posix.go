//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

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

func startAdapter(cmd *exec.Cmd) (func(), error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, nil
}
