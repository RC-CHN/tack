package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func shellCommand(script string) p.Command {
	mode := "posix-sh"
	if runtime.GOOS == "windows" {
		mode = "cmd"
	}
	return p.Command{Mode: mode, Script: script, TimeoutSeconds: 5, OutputLimit: 1024}
}

func TestOutputExitAndEnvironment(t *testing.T) {
	script := `printf '%s' "$TACK_TEST_VALUE"; printf problem >&2; exit 7`
	if runtime.GOOS == "windows" {
		script = "@echo off\r\necho %TACK_TEST_VALUE%\r\necho problem 1>&2\r\nexit /b 7\r\n"
	}
	c := shellCommand(script)
	c.Env = map[string]string{"TACK_TEST_VALUE": "spaces and $literal"}
	state, result := (Runner{WorkDir: t.TempDir()}).Run(context.Background(), c)
	if state != "failed" || result.ExitCode == nil || *result.ExitCode != 7 || !strings.Contains(string(result.Stdout), "spaces and $literal") || !strings.Contains(string(result.Stderr), "problem") {
		t.Fatalf("%s %+v", state, result)
	}
}

func TestOutputBoundedAndDrained(t *testing.T) {
	script := `i=0; while [ "$i" -lt 10000 ]; do printf abcdefghij; i=$((i+1)); done`
	if runtime.GOOS == "windows" {
		script = "@echo off\r\nfor /L %%i in (1,1,10000) do echo abcdefghij\r\n"
	}
	c := shellCommand(script)
	c.OutputLimit = 31
	state, result := (Runner{WorkDir: t.TempDir()}).Run(context.Background(), c)
	if state != "succeeded" || len(result.Stdout) != 31 || !result.StdoutTruncated {
		t.Fatalf("%s %+v", state, result)
	}
}

func TestCancelAndTimeout(t *testing.T) {
	script := "sleep 30"
	if runtime.GOOS == "windows" {
		script = "@ping -n 30 127.0.0.1 >nul\r\n"
	}
	for _, cancelEarly := range []bool{false, true} {
		c := shellCommand(script)
		c.TimeoutSeconds = 1
		ctx, cancel := context.WithCancel(context.Background())
		if cancelEarly {
			time.AfterFunc(100*time.Millisecond, cancel)
		}
		start := time.Now()
		state, result := (Runner{WorkDir: t.TempDir()}).Run(ctx, c)
		cancel()
		want := "timed_out"
		if cancelEarly {
			want = "canceled"
		}
		if state != want || time.Since(start) > 5*time.Second {
			t.Fatalf("%s %+v", state, result)
		}
	}
}

func TestExplicitShellDoesNotFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fake interpreter")
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "unexpected")
	if err := os.WriteFile(filepath.Join(bin, "busybox"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	c := p.Command{Mode: "bash", Script: "echo ok", TimeoutSeconds: 1, OutputLimit: 100}
	state, _ := (Runner{WorkDir: t.TempDir(), BusyBox: filepath.Join(bin, "busybox")}).Run(context.Background(), c)
	if state != "failed" {
		t.Fatal(state)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("fallback interpreter executed")
	}
}

func TestDirectArgumentsAreNotShellExpanded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix printf")
	}
	c := p.Command{Mode: "exec", Program: "printf", Args: []string{"%s", "$(echo unsafe); literal"}, TimeoutSeconds: 1, OutputLimit: 100}
	state, result := (Runner{WorkDir: t.TempDir()}).Run(context.Background(), c)
	if state != "succeeded" || string(result.Stdout) != "$(echo unsafe); literal" {
		t.Fatalf("%s %+v", state, result)
	}
}

func TestBusyBoxFallbackWhenShIsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fake BusyBox")
	}
	bin := t.TempDir()
	busybox := filepath.Join(bin, "busybox")
	if err := os.WriteFile(busybox, []byte("#!/bin/sh\n[ \"$1\" = ash ] || exit 91\nshift\nexec /bin/sh \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	c := shellCommand("printf portable-fallback")
	state, result := (Runner{WorkDir: t.TempDir(), BusyBox: busybox}).Run(context.Background(), c)
	if state != "succeeded" || result.Executable != busybox || string(result.Stdout) != "portable-fallback" {
		t.Fatalf("%s %+v", state, result)
	}
}

func TestPowerShellUnicodeScript(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell script encoding")
	}
	c := p.Command{Mode: "powershell", Script: "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)\n[Console]::Write('客户')", TimeoutSeconds: 15, OutputLimit: 1024}
	state, result := (Runner{WorkDir: t.TempDir()}).Run(context.Background(), c)
	if state != "succeeded" || string(result.Stdout) != "客户" {
		t.Fatalf("%s %+v", state, result)
	}
}
