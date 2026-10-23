package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// Adapted from Queqiao's malformed-frame/ACK conformance approach to Tack's
// versioned JSON and output manifests. These do not import its wire protocol.
func FuzzManagementDecode(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `[]`, `{"protocol_version":3}`, `{"offset":-1,"data":"AA=="}`, `{"offset":0,"data":"!!!"}`, `{} {}`, `{"unknown":true}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > MaxBody {
			t.Skip()
		}
		var chunk ResultChunk
		if Decode(io.LimitReader(bytes.NewReader(input), MaxBody), &chunk) == nil {
			// Accepted input must survive canonical encode/decode without changing
			// the byte offset or binary payload, including non-UTF8 output.
			canonical, err := json.Marshal(chunk)
			if err != nil {
				t.Fatal(err)
			}
			var again ResultChunk
			if err = Decode(bytes.NewReader(canonical), &again); err != nil || again.Offset != chunk.Offset || !bytes.Equal(again.Data, chunk.Data) {
				t.Fatalf("chunk roundtrip: %v", err)
			}
		}
		var request SyncRequest
		_ = Decode(io.LimitReader(bytes.NewReader(input), MaxBody), &request)
	})
}

func FuzzOutputManifestBoundaries(f *testing.F) {
	f.Add(0, 0, "")
	f.Add(-1, 1, "bad")
	f.Add(MaxOutput, MaxOutput, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	f.Fuzz(func(t *testing.T, stdout, stderr int, digest string) {
		if len(digest) > 256 {
			t.Skip()
		}
		m := Manifest{StdoutBytes: stdout, StderrBytes: stderr, SHA256: digest}
		if m.Validate(MaxOutput) == nil {
			if m.Total() < 0 || m.Total() > 2*MaxOutput {
				t.Fatal("accepted manifest overflows output budget")
			}
		}
	})
}
