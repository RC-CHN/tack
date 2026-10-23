package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	p "tack.local/management/protocol"
)

type Runner struct{ WorkDir, BusyBox string }
type bounded struct {
	mu        sync.Mutex
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (b *bounded) Write(v []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(v)
	remaining := b.limit - b.data.Len()
	if n > remaining {
		b.truncated = true
		v = v[:remaining]
	}
	b.data.Write(v)
	return n, nil
}
func (b *bounded) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data.Bytes()...), b.truncated
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
	result := &p.Result{StartedAt: time.Now().UTC()}
	state := "failed"
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err := c.Validate(); err != nil {
		result.Error = err.Error()
		return state, result
	}
	workspace, err := os.MkdirTemp(r.WorkDir, "task-")
	if err != nil {
		result.Error = err.Error()
		return state, result
	}
	defer os.RemoveAll(workspace)
	path, args := c.Program, append([]string(nil), c.Args...)
	if c.Mode == "exec" {
		if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(path), ".cmd") || strings.EqualFold(filepath.Ext(path), ".bat")) {
			result.Error = "use cmd mode for batch files"
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
			err = os.WriteFile(file, script, 0600)
			args = append(args, file)
		}
	}
	if err != nil {
		result.Error = err.Error()
		return state, result
	}
	result.Executable = path
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
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
	err = runProcess(ctx, cmd, c.Mode)
	result.Stdout, result.StdoutTruncated = out.snapshot()
	result.Stderr, result.StderrTruncated = errout.snapshot()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		state = "timed_out"
	case ctx.Err() != nil:
		state = "canceled"
	case err == nil:
		state = "succeeded"
	}
	if err != nil {
		result.Error = err.Error()
	}
	return state, result
}
