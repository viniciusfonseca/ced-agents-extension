package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func pollablePipe(original *os.File) (*os.File, error) {
	// CED supplies inherited Windows pipe handles on stdin and stdout.
	return original, nil
}

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	createJob    = kernel32.NewProc("CreateJobObjectW")
	assignToJob  = kernel32.NewProc("AssignProcessToJobObject")
	terminateJob = kernel32.NewProc("TerminateJobObject")
)

func startAdapter(cmd *exec.Cmd) (func(), error) {
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("create adapter job: %w", err)
	}
	const createNoWindow = 0x08000000 // CREATE_NO_WINDOW
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return nil, err
	}
	const processSetQuota = 0x100
	handle, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		ok, _, assignErr := assignToJob.Call(job, uintptr(handle))
		_ = syscall.CloseHandle(handle)
		if ok == 0 {
			err = assignErr
		}
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = syscall.CloseHandle(syscall.Handle(job))
		return nil, fmt.Errorf("assign adapter to job: %w", err)
	}
	return func() {
		_, _, _ = terminateJob.Call(job, 1)
		_ = syscall.CloseHandle(syscall.Handle(job))
	}, nil
}

func agentCommand(agent provider) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("Agents requires an absolute USERPROFILE to find installed adapters.")
	}
	appData, localAppData := os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(appData) {
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	if !filepath.IsAbs(localAppData) {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	npm := filepath.Join(appData, "npm")
	dirs := []string{filepath.Join(home, ".local", "bin"), npm}
	system := os.Getenv("SystemRoot")
	systemDrive := filepath.VolumeName(system)
	for _, location := range []struct{ env, name string }{
		{"ProgramFiles", "Program Files"}, {"ProgramFiles(x86)", "Program Files (x86)"},
	} {
		dir := os.Getenv(location.env)
		if !filepath.IsAbs(dir) && systemDrive != "" {
			dir = filepath.Join(systemDrive+string(filepath.Separator), location.name)
		}
		if filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.Join(dir, "nodejs"))
		}
	}
	if filepath.IsAbs(system) {
		dirs = append(dirs, filepath.Join(system, "System32"), system)
	}

	var cmd *exec.Cmd
	for _, dir := range dirs {
		executable := filepath.Join(dir, agent.command+".exe")
		if target, err := exec.LookPath(executable); err == nil {
			cmd = exec.Command(target, agent.args...)
			break
		}
	}
	if cmd == nil {
		for _, dir := range dirs {
			node, err := exec.LookPath(filepath.Join(dir, "node.exe"))
			if err == nil {
				if script := npmAdapterScript(npm, agent.command); script != "" {
					cmd = exec.Command(node, append([]string{script}, agent.args...)...)
				}
				break
			}
		}
	}
	if cmd == nil {
		return nil, fmt.Errorf("Install %s and make %s.exe available in %%USERPROFILE%%\\.local\\bin or install its npm adapter with Node.js.", agent.Name, agent.command)
	}
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + appData, "LOCALAPPDATA=" + localAppData,
		"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
	for _, key := range []string{"TEMP", "TMP", "SystemRoot", "WINDIR", "LANG", "LC_ALL", "LC_CTYPE"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	return cmd, nil
}

func npmAdapterScript(prefix, command string) string {
	if command != "codex-acp" && command != "claude-agent-acp" {
		return ""
	}
	root := filepath.Join(prefix, "node_modules", "@agentclientprotocol", command)
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var manifest struct{ Bin json.RawMessage }
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	var script string
	if json.Unmarshal(manifest.Bin, &script) != nil {
		var entries map[string]string
		if json.Unmarshal(manifest.Bin, &entries) != nil {
			return ""
		}
		script = entries[command]
	}
	if script == "" || !filepath.IsLocal(script) {
		return ""
	}
	path := filepath.Join(root, script)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}
