package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	p "tack.local/management/protocol"
)

type Runner struct {
	WorkDir, BusyBox, Owner string
	removeAll               func(string) error
}

func (r Runner) executable(mode string) (string, []string, error) {
	find := func(name string) (string, []string, error) { path, err := exec.LookPath(name); return path, nil, err }
	switch mode {
	case "portable-sh":
		if r.BusyBox == "" || !filepath.IsAbs(r.BusyBox) {
			return "", nil, errors.New("portable-sh requires configured absolute BusyBox path")
		}
		path, err := exec.LookPath(r.BusyBox)
		if err != nil {
			return "", nil, err
		}
		return path, []string{"ash"}, nil
	case "posix-sh":
		if runtime.GOOS != "windows" {
			if path, _, err := find("sh"); err == nil {
				return path, nil, nil
			}
		}
		return r.executable("portable-sh")
	case "bash", "zsh":
		return find(mode)
	case "powershell":
		path, _, err := find("pwsh")
		if err != nil {
			path, _, err = find("powershell")
		}
		return path, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-File"}, err
	case "cmd":
		if runtime.GOOS != "windows" {
			return "", nil, errors.New("cmd is only supported on Windows")
		}
		path, _, err := find("cmd.exe")
		return path, []string{"/d", "/s", "/c"}, err
	}
	return "", nil, fmt.Errorf("unsupported mode %q", mode)
}

func (r Runner) Modes() []string {
	modes := []string{"exec"}
	for _, mode := range []string{"posix-sh", "portable-sh", "bash", "zsh", "powershell", "cmd"} {
		if _, _, err := r.executable(mode); err == nil {
			modes = append(modes, mode)
		}
	}
	return modes
}

// Run resolves the interpreter once and never retries a partially executed script.
func (r Runner) Run(ctx context.Context, c p.Command) (string, *p.Result) {
	return r.RunBefore(ctx, c, nil)
}

func (r Runner) RunBefore(ctx context.Context, c p.Command, deadline *time.Time) (string, *p.Result) {
	result := &p.Result{Phase: "validation"}
	state := "failed"
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := c.Validate(); err != nil {
		result.Error, result.ErrorCode = err.Error(), "invalid_command"
		return state, result
	}
	if p.DeadlineExpired(deadline, time.Now()) {
		return "expired", p.ExpiredResult()
	}
	result.Phase = "workspace"
	root, err := r.Recover()
	if err != nil {
		result.Error, result.ErrorCode = err.Error(), "workspace_io_failed"
		return state, result
	}
	workspace, err := os.MkdirTemp(root, "task-")
	if err != nil {
		result.Error, result.ErrorCode = err.Error(), "workspace_io_failed"
		return state, result
	}
	defer func() {
		remove := r.removeAll
		if remove == nil {
			remove = os.RemoveAll
		}
		result.Cleanup = &p.CleanupResult{}
		if err := remove(workspace); err != nil {
			result.Cleanup = &p.CleanupResult{ErrorCode: "cleanup_failed", Error: err.Error(), Path: workspace}
		}
	}()
	result.Phase, result.ErrorCode = "resolve", "interpreter_missing"
	path, args := c.Program, append([]string(nil), c.Args...)
	if c.Mode == "exec" {
		result.ErrorCode = "program_missing"
		if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(path), ".cmd") || strings.EqualFold(filepath.Ext(path), ".bat")) {
			result.Error, result.ErrorCode = "use cmd mode for batch files", "invalid_command"
			return state, result
		}
		path, err = exec.LookPath(path)
	} else {
		path, args, err = r.executable(c.Mode)
		if err == nil {
			ext := ".sh"
			if c.Mode == "powershell" {
				ext = ".ps1"
			}
			if c.Mode == "cmd" {
				ext = ".cmd"
			}
			file := filepath.Join(workspace, "script"+ext)
			script := []byte(c.Script)
			if c.Mode == "powershell" && runtime.GOOS == "windows" {
				// Windows PowerShell 5.1 otherwise interprets BOM-less files
				// using the system ANSI code page. PowerShell 7 also accepts this.
				script = append([]byte{0xef, 0xbb, 0xbf}, script...)
			}
			result.Phase, result.ErrorCode = "workspace", "script_write_failed"
			err = os.WriteFile(file, script, 0600)
			args = append(args, file)
		}
	}
	if err != nil {
		result.Error = err.Error()
		return state, result
	}
	result.Executable = path
	result.Phase, result.ErrorCode = "start", ""
	if err := ctx.Err(); err != nil {
		result.Error, result.ErrorCode = err.Error(), "command_canceled"
		return "canceled", result
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = c.Dir
	if cmd.Dir == "" {
		cmd.Dir = workspace
	}
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+c.Env[k])
	}
	out, errout := &bounded{limit: c.OutputLimit}, &bounded{limit: c.OutputLimit}
	cmd.Stdout, cmd.Stderr = out, errout
	if p.DeadlineExpired(deadline, time.Now()) {
		result.ErrorCode, result.Phase, result.Error = "start_deadline_exceeded", "queued", "latest allowed start time has passed; command was not started"
		return "expired", result
	}
	result.StartedAt = time.Now().UTC()
	err = runProcess(ctx, cmd, c.Mode)
	result.Stdout, result.StdoutTruncated = out.finish()
	result.Stderr, result.StderrTruncated = errout.finish()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		state, result.ErrorCode, result.Phase = "timed_out", "command_timeout", "execute"
	case ctx.Err() != nil:
		state, result.ErrorCode, result.Phase = "canceled", "command_canceled", "execute"
	case err == nil:
		state, result.Phase = "succeeded", "finished"
	case cmd.ProcessState != nil && !cmd.ProcessState.Success():
		result.ErrorCode, result.Phase = "command_exit_nonzero", "execute"
	case cmd.Process == nil || cmd.ProcessState == nil:
		result.ErrorCode, result.Phase = "process_start_failed", "start"
	default:
		result.ErrorCode, result.Phase = "process_wait_failed", "execute"
	}
	if err != nil {
		result.Error = err.Error()
	}
	return state, result
}
