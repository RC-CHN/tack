package protocol

import "testing"

func TestManifestValidation(t *testing.T) {
	m := OutputManifest(&Result{Stdout: []byte("a"), Stderr: []byte("bc")})
	if m.Total() != 3 || m.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || m.Validate(2) != nil {
		t.Fatalf("%+v", m)
	}
	if m.Validate(1) == nil {
		t.Fatal("allowed oversized stream")
	}
	m.StdoutBytes = -1
	if m.Validate(2) == nil {
		t.Fatal("negative stream accepted")
	}
	m = OutputManifest(&Result{})
	m.SHA256 = "../bad"
	if m.Validate(MaxOutput) == nil {
		t.Fatal("invalid digest accepted")
	}
}
