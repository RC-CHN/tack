package protocol

import (
	"strings"
	"testing"
)

func TestTransportPolicy(t *testing.T) {
	for _, url := range []string{"http://example.com", "http://localhost:8080", "https://user:secret@example.com", "https://example.com/path", "https://example.com?secret=x"} {
		if _, _, err := HTTPClient(url, "", true); err == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	if _, _, err := HTTPClient("http://127.0.0.1:8080", "", false); err == nil {
		t.Fatal("plaintext without explicit opt-in")
	}
	if _, _, err := HTTPClient("http://127.0.0.1:8080", "", true); err != nil {
		t.Fatal(err)
	}
}

func TestCommandBoundaries(t *testing.T) {
	for _, command := range []Command{
		{Mode: "exec", Program: "echo", Script: "echo unexpected"},
		{Mode: "bash", Script: "echo ok", Args: []string{"extra"}},
		{Mode: "exec", Program: "echo", Env: map[string]string{"KEY=OTHER": "bad"}},
		{Mode: "exec", Program: "echo", TimeoutSeconds: 86401},
	} {
		command.Defaults()
		if command.Validate() == nil {
			t.Fatalf("accepted invalid command: %+v", command)
		}
	}
	var command Command
	if Decode(strings.NewReader(`{"mode":"exec","unknown":true}`), &command) == nil {
		t.Fatal("unknown field accepted")
	}
	if Decode(strings.NewReader(`{} {}`), &command) == nil {
		t.Fatal("trailing object accepted")
	}
}
