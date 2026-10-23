package tunnel

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type fragmentWriter struct {
	bytes.Buffer
	maximum int
}

func (w *fragmentWriter) Write(b []byte) (int, error) {
	return w.Buffer.Write(b[:min(len(b), w.maximum)])
}

func TestSOCKSWritesHandleFragmentsAndRejectZeroProgress(t *testing.T) {
	for _, step := range []int{1, 3, 8} {
		writer := &fragmentWriter{maximum: step}
		payload := bytes.Repeat([]byte("frame"), 17)
		if err := writeSocksBytes(writer, payload); err != nil || !bytes.Equal(writer.Bytes(), payload) {
			t.Fatal("short writer lost bytes", step, err)
		}
	}
	writer := &fragmentWriter{}
	if err := writeSocksBytes(writer, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("zero-progress writer accepted", err)
	}
}

func FuzzSOCKSRequestAddress(f *testing.F) {
	f.Add([]byte{1, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{3, 1, 'x', 0, 1})
	f.Add([]byte{3, 0})
	f.Add([]byte{255})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024 {
			t.Skip()
		}
		reader := bytes.NewReader(data)
		address, err := readSocksAddress(reader)
		if err != nil {
			return
		}
		if len(address.wire) > 259 || len(address.wire) > len(data) || len(address.wire) != len(data)-reader.Len() {
			t.Fatal("request address escaped bounds")
		}
		again, err := readSocksAddress(bytes.NewReader(address.wire))
		if err != nil || address.host != again.host || address.port != again.port {
			t.Fatal("accepted address changed on round trip")
		}
	})
}
