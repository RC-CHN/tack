//go:build linux

package resource

import (
	"os"
	"testing"
)

func TestReadLiveProcessAndRejectMissingProcess(t *testing.T) {
	s, err := Process(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if s.RSS == 0 || s.PeakRSS < s.RSS || s.FDs < 3 || s.Threads == 0 {
		t.Fatalf("invalid sample: %+v", s)
	}
	if _, err := Process(-1); err == nil {
		t.Fatal("missing process was reported as zero usage")
	}
}
