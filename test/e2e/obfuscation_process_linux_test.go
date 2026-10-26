//go:build linux

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	chserver "github.com/jpillora/chisel/server"
	"tack.local/testfixtures/faultnet"
	"tack.local/testfixtures/resource"
)

// Keep selected listener ports out of the kernel's ephemeral source-port range
// and reserved until immediately before bind. Concurrent fixtures otherwise
// race an outbound TLS socket between availablePort and subprocess startup.
func reserveProcessPort(t *testing.T) (string, net.Listener) {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatal("invalid ephemeral port range")
	}
	low, e1 := strconv.Atoi(fields[0])
	high, e2 := strconv.Atoi(fields[1])
	if e1 != nil || e2 != nil {
		t.Fatal("invalid ephemeral port range")
	}
	for i := 0; i < 256; i++ {
		port := availablePort()
		number, _ := strconv.Atoi(port)
		if number >= low && number <= high {
			continue
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:"+port)
		if err == nil {
			return port, listener
		}
	}
	t.Fatal("could not reserve a listener port outside the ephemeral range")
	return "", nil
}

// External client only: server, echo target and impairment workers are outside
// the process whose RSS/CPU is sampled. Each outage waits for recovery before
// the next; this is NOT the Node fixture's fixed four-second outage cadence.
func TestObfuscationProcess(t *testing.T) {
	binary := os.Getenv("TACK_CHISEL_BINARY")
	if binary == "" {
		t.Skip("set TACK_CHISEL_BINARY and TACK_CHISEL_REPORT")
	}
	build, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range build.Settings {
		if setting.Key == "-race" && setting.Value == "true" {
			t.Fatal("race binary cannot measure footprint")
		}
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	data = nil
	profile, shape := os.Getenv("TACK_TLS_PROFILE"), os.Getenv("TACK_TRAFFIC_SHAPING")
	report := map[string]any{"format": "tack-chisel-obfuscation-v1", "tls_profile": profile, "traffic_shaping": shape, "binary_sha256": hex.EncodeToString(sum[:]), "go": build.GoVersion, "one_way_delay_ms": 300, "rate_bytes_per_second": 8192, "outage_duration_ms": 500, "outages": 3, "payload_bytes": 1024, "idle_seconds": 5}
	defer func() {
		report["passed"] = !t.Failed()
		if path := os.Getenv("TACK_CHISEL_REPORT"); path != "" {
			b, err := json.MarshalIndent(report, "", "  ")
			if err == nil {
				err = os.WriteFile(path, append(b, '\n'), 0600)
			}
			if err != nil {
				t.Error(err)
			}
		}
	}()
	certs, err := newTestTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer certs.Close()
	certs.serverTLS.CA = ""
	certs.serverTLS.TrafficShaping = shape
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 2048))
		if err == nil {
			w.Write(b)
		}
	}))
	defer origin.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := chserver.NewServer(&chserver.Config{TLS: *certs.serverTLS, Auth: "fixture:fixture-only", KeepAlive: 0})
	if err != nil {
		t.Fatal(err)
	}
	server.SetOutput(io.Discard)
	port, serverReservation := reserveProcessPort(t)
	serverReservation.Close()
	if err = server.StartContext(ctx, "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); server.Wait() }()
	link, err := faultnet.NewConfigured("127.0.0.1:"+port, faultnet.Options{Upload: faultnet.Traffic{Delay: 300 * time.Millisecond, BytesPerSecond: 8192}, Download: faultnet.Traffic{Delay: 300 * time.Millisecond, BytesPerSecond: 8192}, QueueChunks: 2, ChunkBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	_, relayPort, _ := net.SplitHostPort(link.Addr())
	local, clientReservation := reserveProcessPort(t)
	defer clientReservation.Close()
	args := []string{"client", "--keepalive=0s", "--max-retry-interval=200ms", "--min-retry-interval=50ms", "--auth=fixture:fixture-only", "--tls-ca=" + certs.clientTLS.CA, "--tls-profile=" + profile, "--traffic-shaping=" + shape, "--fingerprint=" + server.GetFingerprint(), "https://localhost:" + relayPort, "127.0.0.1:" + local + ":" + strings.TrimPrefix(origin.URL, "http://")}
	if profile == "personalized-v1" {
		args = append(args[:1], append([]string{"--tls-identity-file=" + filepath.Join(t.TempDir(), "tls-identity.json")}, args[1:]...)...)
	}
	cmd := exec.Command(binary, args...)
	// Logs are finite in this fixture; retaining them helps diagnose failures.
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	clientReservation.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-done
		}
		report["client_log"] = logs.String()
	}()
	var samples []resource.Sample
	sample := func() resource.Sample {
		s, err := resource.Process(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
		return s
	}
	defer func() {
		report["samples"] = samples
		var peak uint64
		for _, s := range samples {
			peak = max(peak, s.RSS, s.PeakRSS)
		}
		report["max_rss_bytes"] = peak
		if peak > 32<<20 {
			t.Error("client RSS exceeded 32 MiB")
		}
	}()
	httpClient := &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer httpClient.CloseIdleConnections()
	payload := bytes.Repeat([]byte{0x42}, 1024)
	probe := func() bool {
		r, err := httpClient.Post("http://127.0.0.1:"+local, "application/octet-stream", bytes.NewReader(payload))
		if err != nil {
			return false
		}
		defer r.Body.Close()
		b, err := io.ReadAll(io.LimitReader(r.Body, 2048))
		return err == nil && bytes.Equal(b, payload)
	}
	wait := func() float64 {
		start := time.Now()
		for time.Since(start) < 30*time.Second {
			sample()
			if probe() {
				sample() // Include the completed handshake/transfer's VmHWM too.
				return time.Since(start).Seconds()
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("tunnel failed to recover and echo within 30 seconds")
		return 0
	}
	report["startup_and_echo_seconds"] = wait()
	// Let pending ACKs settle before observing quiet idle. No periodic cover
	// traffic is requested, and SSH keepalive is explicitly disabled.
	time.Sleep(time.Second)
	idleStart, networkStart := sample(), link.Snapshot()
	report["idle_start"], report["idle_network_start"] = idleStart, networkStart
	time.Sleep(5 * time.Second)
	idleEnd, networkEnd := sample(), link.Snapshot()
	report["idle_end"], report["idle_network_end"] = idleEnd, networkEnd
	if idleEnd.CPUTicks-idleStart.CPUTicks > 2 || idleEnd.WriteBytes != idleStart.WriteBytes || idleEnd.WriteChars != idleStart.WriteChars || networkEnd.UpBytes != networkStart.UpBytes || networkEnd.DownBytes != networkStart.DownBytes {
		t.Error("quiet idle budget exceeded")
	}
	var recovery []float64
	for i := 0; i < 3; i++ {
		link.SetMode(faultnet.Offline)
		time.Sleep(500 * time.Millisecond)
		link.SetMode(faultnet.Online)
		recovery = append(recovery, wait())
		report["recovery_and_echo_seconds"] = recovery
	}
	report["completed"] = len(recovery) == 3
	report["network_end"] = link.Snapshot()
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client shutdown blocked")
	}
	report["shutdown_ms"] = float64(time.Since(start)) / float64(time.Millisecond)
}
