package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestStructuredExecutionFailures(t *testing.T) {
	work := t.TempDir()
	cases := []struct {
		name              string
		command           p.Command
		work, code, phase string
	}{
		{"validation", p.Command{}, work, "invalid_command", "validation"},
		{"program", p.Command{Mode: "exec", Program: filepath.Join(work, "absent"), TimeoutSeconds: 1, OutputLimit: 20}, work, "program_missing", "resolve"},
		{"interpreter", p.Command{Mode: "portable-sh", Script: "echo hi", TimeoutSeconds: 1, OutputLimit: 20}, work, "interpreter_missing", "resolve"},
		{"workspace", shellCommand("echo hi"), filepath.Join(work, "absent", "work"), "workspace_io_failed", "workspace"},
		{"start", p.Command{Mode: "exec", Program: os.Args[0], Dir: filepath.Join(work, "absent"), TimeoutSeconds: 1, OutputLimit: 20}, work, "process_start_failed", "start"},
		{"exit", shellCommand("exit 7"), work, "command_exit_nonzero", "execute"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state, result := (Runner{WorkDir: tt.work}).Run(context.Background(), tt.command)
			if state != "failed" || result.ErrorCode != tt.code || result.Phase != tt.phase {
				t.Fatalf("%s %+v", state, result)
			}
		})
	}
}
func TestSuccessfulCommandWithFailedCleanup(t *testing.T) {
	r := Runner{WorkDir: t.TempDir(), removeAll: func(string) error { return errors.New("injected cleanup failure") }}
	state, result := r.Run(context.Background(), shellCommand("echo success"))
	if state != "succeeded" || result.Error != "" || result.ErrorCode != "" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("execution contaminated: %s %+v", state, result)
	}
	if result.Cleanup == nil || result.Cleanup.ErrorCode != "cleanup_failed" || result.Cleanup.Path == "" {
		t.Fatal("cleanup diagnosis missing", result)
	}
	if _, err := os.Stat(result.Cleanup.Path); err != nil {
		t.Fatal("path not preserved", err)
	}
}
func TestExpiredDeadlineDoesNotStartOrCreateWorkspace(t *testing.T) {
	work := t.TempDir()
	deadline := time.Now().Add(-time.Second)
	state, result := (Runner{WorkDir: work}).RunBefore(context.Background(), shellCommand("echo must-not-run"), &deadline)
	entries, _ := os.ReadDir(work)
	if state != "expired" || result.ErrorCode != "start_deadline_exceeded" || result.ExitCode != nil || !result.StartedAt.IsZero() || len(entries) != 0 {
		t.Fatalf("%s %+v entries=%d", state, result, len(entries))
	}
}
