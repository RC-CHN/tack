package browser

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"tack.local/management/protocol/shaping"
	"tack.local/management/protocol/tlsprofile"
)

// Isolate the bounded-v1 frame overhead over TLS; actual application paths
// are covered separately by Node and Chisel integration fixtures.
func TestEarlyPaddingTLSExperiment(t *testing.T) {
	certServer, cfg := tlsServer(t)
	type result struct {
		Padded        bool    `json:"padded"`
		Bytes         int     `json:"payload_bytes"`
		Micros        []int64 `json:"echo_us"`
		WireBytes     []int   `json:"upstream_application_tls_bytes"`
		RecordLengths [][]int `json:"upstream_tls_record_payload_lengths"`
		ConnBytes     uintptr `json:"padding_conn_struct_bytes"`
	}
	var results []result
	for _, padded := range []bool{false, true} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				raw, err := l.Accept()
				if err != nil {
					return
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer raw.Close()
					raw.SetDeadline(time.Now().Add(5 * time.Second))
					var conn net.Conn = tls.Server(raw, certServer.TLS.Clone())
					if padded {
						conn = shaping.NewClient(conn)
					}
					io.Copy(conn, conn)
				}()
			}
		}()
		out := result{Padded: padded, Bytes: 32768, ConnBytes: unsafe.Sizeof(shaping.Conn{})}
		for i := 0; i < 20; i++ {
			var wire *capture
			dial, err := tlsprofile.DialTLS(Profile, func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				wire = &capture{Conn: c}
				return wire, nil
			}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := dial(ctx, "tcp", l.Addr().String())
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			if padded {
				conn = shaping.NewClient(conn)
			}
			startBytes := len(wire.bytes)
			payload := bytes.Repeat([]byte{53}, out.Bytes)
			start := time.Now()
			writeDone := make(chan error, 1)
			go func() { _, err := conn.Write(payload); writeDone <- err }()
			response := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatal(err)
			}
			if err := <-writeDone; err != nil || !bytes.Equal(payload, response) {
				t.Fatal("bad echo", err)
			}
			out.Micros = append(out.Micros, time.Since(start).Microseconds())
			wire.mu.Lock()
			data := wire.bytes[startBytes:]
			out.WireBytes = append(out.WireBytes, len(data))
			var lengths []int
			for len(data) >= 5 {
				n := int(binary.BigEndian.Uint16(data[3:5]))
				if len(data) < 5+n {
					t.Fatal("incomplete TLS record")
				}
				lengths = append(lengths, n)
				data = data[5+n:]
			}
			wire.mu.Unlock()
			out.RecordLengths = append(out.RecordLengths, lengths)
			conn.Close()
		}
		l.Close()
		workers.Wait()
		results = append(results, out)
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	t.Log(string(data))
	if path := os.Getenv("TACK_PADDING_REPORT"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
