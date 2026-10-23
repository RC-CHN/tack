package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	socksserver "github.com/armon/go-socks5"
	"github.com/jpillora/chisel/test/campaign"
)

type remoteResolver struct{ names chan string }

func (r remoteResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	r.names <- name
	return ctx, net.IPv4(127, 0, 0, 1), nil
}

func TestSOCKSHTTPResolvesAtProxyAndRejectsUntrustedTLS(t *testing.T) {
	resolver := remoteResolver{names: make(chan string, 2)}
	proxy, err := socksserver.New(&socksserver.Config{Resolver: resolver, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go proxy.Serve(listener)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer origin.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	c := config{Targets: []campaign.Target{{Name: "remote-dns", URL: "http://remote-only.invalid:" + port, Bytes: -1}}, Endpoints: []endpoint{{Name: "socks", SOCKS: listener.Addr().String()}}, Rounds: 1, MinSuccess: 1, TimeoutMS: 1000}
	if err = run(context.Background(), c, filepath.Join(t.TempDir(), "dns"), "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-resolver.names:
		if name != "remote-only.invalid" {
			t.Fatal(name)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS proxy did not resolve destination")
	}
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer secure.Close()
	c.Targets[0].URL = secure.URL
	if run(context.Background(), c, filepath.Join(t.TempDir(), "tls"), "test") == nil {
		t.Fatal("TLS certificate verification disabled")
	}
}

func TestConfigRejectsUnsafeAmbiguousAndUnboundedCampaigns(t *testing.T) {
	for _, target := range []campaign.Target{{Name: "../secret", URL: "https://example.invalid", Bytes: -1}, {Name: "safe", URL: "file:///etc/passwd", Bytes: -1}, {Name: "safe", URL: "https://user:password@example.invalid", Bytes: -1}, {Name: "safe", URL: "https://example.invalid", Bytes: 10}} {
		c := config{Targets: []campaign.Target{target}}
		if c.validate() == nil {
			t.Fatal("accepted invalid target", target.Name)
		}
	}
	for _, value := range []string{"localhost", "localhost:0", "localhost:65536", "[::1]:-1"} {
		if address(value) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"127.0.0.1:53", "[::1]:1053", "resolver.invalid:53"} {
		if !address(value) {
			t.Fatal(value)
		}
	}
}

func TestLocalCampaignProducesVerifiableEvidenceAndKeepsOneUDPAssociation(t *testing.T) {
	body := []byte("small benchmark object")
	sum := sha256.Sum256(body)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer s.Close()
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	sources := make(chan string, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for i := 0; i < 3; i++ {
			n, addr, err := u.ReadFrom(buf)
			if err != nil {
				return
			}
			sources <- addr.String()
			u.WriteTo(buf[:n], addr)
		}
	}()
	c := config{Targets: []campaign.Target{{Name: "object", URL: s.URL, Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}, UDPEcho: u.LocalAddr().String(), Rounds: 3, Flows: 2, TimeoutMS: 1000, MinSuccess: 1, FinalSuccesses: 2}
	out := filepath.Join(t.TempDir(), "evidence")
	if err = run(context.Background(), c, out, "test-config-hash"); err != nil {
		t.Fatal(err)
	}
	<-done
	close(sources)
	first := ""
	for source := range sources {
		if first == "" {
			first = source
		}
		if source != first {
			t.Fatal("association replaced between probes")
		}
	}
	data, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Passed    bool
		Rounds    int
		Summaries map[string]campaign.Summary
	}
	if err = json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Passed || summary.Rounds != 3 || summary.Summaries["direct/object"].Completed != 6 || summary.Summaries["direct/udp-echo"].Completed != 3 {
		t.Fatal(string(data))
	}
	manifest, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		fields := strings.Fields(line)
		data, err := os.ReadFile(filepath.Join(out, fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != fields[0] {
			t.Fatal("bad checksum")
		}
	}
	if run(context.Background(), c, out, "test") == nil {
		t.Fatal("overwrote existing evidence")
	}
}

func TestFailedCampaignStillWritesSummaryAndChecksums(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer s.Close()
	c := config{Targets: []campaign.Target{{Name: "unavailable", URL: s.URL, Bytes: -1}}, Rounds: 2, MinSuccess: 1, TimeoutMS: 1000}
	out := filepath.Join(t.TempDir(), "failed")
	if run(context.Background(), c, out, "test") == nil {
		t.Fatal("all failed campaign passed")
	}
	data, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err = json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary["passed"] != false || summary["all_failed_rounds"] != float64(2) {
		t.Fatal(string(data))
	}
	if _, err = os.Stat(filepath.Join(out, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedSOCKSAssociationClosesTheSetupSocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		var greeting [3]byte
		if _, err = io.ReadFull(c, greeting[:]); err != nil {
			done <- err
			return
		}
		if _, err = c.Write([]byte{5, 0}); err != nil {
			done <- err
			return
		}
		var request [10]byte
		if _, err = io.ReadFull(c, request[:]); err != nil {
			done <- err
			return
		}
		if _, err = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			done <- err
			return
		}
		_, err = c.Read(make([]byte, 1))
		if err != io.EOF {
			done <- fmt.Errorf("setup socket not released: %v", err)
			return
		}
		done <- nil
	}()
	c, err := openUDP(context.Background(), endpoint{Name: "refused", SOCKS: l.Addr().String()}, "127.0.0.1:53", time.Second)
	if err == nil {
		c.Close()
		t.Fatal("refused SOCKS reply accepted")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMatchedCampaignAlternatesAndEvaluatesBothEndpoints(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer s.Close()
	c := config{Targets: []campaign.Target{{Name: "small", URL: s.URL, Bytes: -1}}, Endpoints: []endpoint{{Name: "reference"}, {Name: "candidate"}}, Rounds: 2, TimeoutMS: 1000, MinSuccess: 1, CompareTo: "reference", MinCompletionRatio: 1}
	out := filepath.Join(t.TempDir(), "comparison")
	if err := run(context.Background(), c, out, "test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "attempts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"reference", "candidate", "candidate", "reference"}
	if len(lines) != len(want) {
		t.Fatal("missing attempts")
	}
	for i, line := range lines {
		var v campaign.Attempt
		if err = json.Unmarshal([]byte(line), &v); err != nil || v.Endpoint != want[i] {
			t.Fatal("endpoint order", i, v, err)
		}
	}
	data, err = os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Comparisons map[string]bool
		Passed      bool
	}
	if err = json.Unmarshal(data, &report); err != nil || !report.Passed || !report.Comparisons["candidate/small"] {
		t.Fatal(string(data), err)
	}
}
