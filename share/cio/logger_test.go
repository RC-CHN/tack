package cio

import (
	"bytes"
	"strings"
	"testing"
)

func TestForkKeepsConfiguredOutput(t *testing.T) {
	var output bytes.Buffer
	parent := NewLogger("node")
	parent.Info = true
	parent.SetOutput(&output)
	parent.Fork("tunnel").Fork("connection").Infof("captured")
	if !strings.Contains(output.String(), "captured") {
		t.Fatal("child logger bypassed the configured bounded destination")
	}
}
