// fieldbench runs explicit, bounded campaigns against already running endpoints.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jpillora/chisel/test/campaign"
	"github.com/txthinking/socks5"
	"golang.org/x/net/proxy"
)

type endpoint struct {
	Name  string `json:"name"`
	SOCKS string `json:"socks,omitempty"`
}
type config struct {
	Targets            []campaign.Target `json:"targets"`
	Endpoints          []endpoint        `json:"endpoints"`
	UDPEcho            string            `json:"udp_echo,omitempty"`
	Rounds             int               `json:"rounds"`
	DurationSeconds    int               `json:"duration_seconds"`
	IntervalMS         int               `json:"interval_ms"`
	TimeoutMS          int               `json:"timeout_ms"`
	Flows              int               `json:"flows"`
	MinSuccess         float64           `json:"min_success"`
	MaxP95MS           float64           `json:"max_p95_ms"`
	FinalSuccesses     uint64            `json:"final_successes"`
	PID                int               `json:"pid,omitempty"`
	MaxRSSBytes        uint64            `json:"max_rss_bytes,omitempty"`
	MetricsURL         string            `json:"metrics_url,omitempty"`
	MetricsTokenEnv    string            `json:"metrics_token_env,omitempty"`
	CompareTo          string            `json:"compare_to,omitempty"`
	MinCompletionRatio float64           `json:"min_completion_ratio,omitempty"`
	MinGoodputRatio    float64           `json:"min_goodput_ratio,omitempty"`
}

func address(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func (c *config) validate() error {
	if c.TimeoutMS == 0 {
		c.TimeoutMS = 10000
	}
	if c.Flows == 0 {
		c.Flows = 1
	}
	if c.Rounds == 0 && c.DurationSeconds == 0 {
		c.Rounds = 1
	}
	if len(c.Endpoints) == 0 {
		c.Endpoints = []endpoint{{Name: "direct"}}
	}
	if len(c.Targets) > 32 || len(c.Endpoints) > 8 || (len(c.Targets) == 0 && c.UDPEcho == "") || c.Flows < 1 || c.Flows > 32 || c.TimeoutMS < 1 || c.TimeoutMS > 120000 || c.IntervalMS < 0 || c.IntervalMS > 3600000 || c.Rounds < 0 || c.Rounds > 1000000 || c.DurationSeconds < 0 || c.DurationSeconds > 604800 || c.MinSuccess < 0 || c.MinSuccess > 1 || c.MaxP95MS < 0 || c.PID < 0 {
		return errors.New("invalid campaign bounds")
	}
	safe := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)
	names := map[string]bool{}
	for _, target := range c.Targets {
		u, err := url.Parse(target.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || !safe.MatchString(target.Name) || names[target.Name] || target.Name == "udp-echo" || target.Bytes < -1 || target.Bytes > 1<<30 {
			return errors.New("invalid/duplicate target; URLs require HTTP(S) without userinfo")
		}
		names[target.Name] = true
		if target.Bytes >= 0 {
			digest, err := hex.DecodeString(target.SHA256)
			if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != target.SHA256 {
				return errors.New("exact transfers require lowercase SHA-256; use bytes=-1 for availability probes")
			}
		}
	}
	clear(names)
	for _, e := range c.Endpoints {
		if !safe.MatchString(e.Name) || names[e.Name] || (e.SOCKS != "" && !address(e.SOCKS)) {
			return errors.New("invalid/duplicate endpoint")
		}
		names[e.Name] = true
	}
	if c.MinCompletionRatio < 0 || c.MinGoodputRatio < 0 || (c.CompareTo != "" && (!names[c.CompareTo] || len(c.Endpoints) < 2)) || ((c.MinCompletionRatio > 0 || c.MinGoodputRatio > 0) && c.CompareTo == "") {
		return errors.New("comparison requires an existing reference and a second endpoint")
	}
	if c.UDPEcho != "" && !address(c.UDPEcho) {
		return errors.New("invalid UDP echo destination")
	}
	if c.MaxRSSBytes > 0 && c.PID == 0 {
		return errors.New("RSS gate requires an external process PID")
	}
	if c.MetricsURL != "" {
		u, err := url.Parse(c.MetricsURL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("invalid metrics URL")
		}
	}
	if c.MetricsTokenEnv != "" && (!regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`).MatchString(c.MetricsTokenEnv) || c.MetricsURL == "" || os.Getenv(c.MetricsTokenEnv) == "") {
		return errors.New("metrics token environment variable is missing or invalid")
	}
	return nil
}

type udpSession struct {
	conn   net.Conn
	failed bool
}

func openUDP(ctx context.Context, e endpoint, target string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if e.SOCKS == "" {
		return dialer.DialContext(ctx, "udp", target)
	}
	client, err := socks5.NewClient(e.SOCKS, "", "", 0, 0)
	if err != nil {
		return nil, err
	}
	var opened []net.Conn
	dial := func(network, local, remote string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, remote)
		if err != nil {
			return nil, err
		}
		conn.SetDeadline(time.Now().Add(timeout))
		opened = append(opened, conn)
		return conn, nil
	}
	client.DialTCP = dial
	client.DialUDP = dial
	conn, err := client.Dial("udp", target)
	if err != nil {
		for _, c := range opened {
			c.Close()
		}
		return nil, err
	}
	for _, c := range opened {
		c.SetDeadline(time.Time{})
	}
	return conn, nil
}

func probeUDP(conn net.Conn, sequence uint64, timeout time.Duration) (out campaign.Attempt) {
	out.Kind, out.Target = "udp", "udp-echo"
	start := time.Now()
	defer func() { out.DurationMS = float64(time.Since(start)) / float64(time.Millisecond) }()
	data := bytes.Repeat([]byte{'u'}, 64)
	binary.BigEndian.PutUint64(data, sequence)
	conn.SetDeadline(time.Now().Add(timeout))
	if n, err := conn.Write(data); err != nil || n != len(data) {
		out.Failure = "udp_write"
		return
	}
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			out.Failure = "udp_read"
			return
		}
		if bytes.Equal(buf[:n], data) {
			out.Success = true
			out.Exact = true
			out.Bytes = int64(n)
			return
		}
		// An old response is not success for a new sequence. The fixed deadline
		// bounds this loop even if delayed or unrelated packets keep arriving.
		if time.Since(start) >= timeout {
			out.Failure = "udp_mismatch"
			return
		}
	}
}

func exclusiveJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func checksumReport(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var lines strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "SHA256SUMS" {
			continue
		}
		f, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(&lines, "%x  %s\n", h.Sum(nil), entry.Name())
	}
	return os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(lines.String()), 0600)
}

func run(ctx context.Context, c config, directory string, configDigest string) (result error) {
	if err := c.validate(); err != nil {
		return err
	}
	if c.DurationSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.DurationSeconds)*time.Second)
		defer cancel()
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	defer func() {
		if err := checksumReport(directory); result == nil {
			result = err
		}
	}()
	build, _ := debug.ReadBuildInfo()
	if err := exclusiveJSON(filepath.Join(directory, "manifest.json"), map[string]any{"format": "tack-fieldbench-v1", "started": time.Now().UTC(), "config_sha256": configDigest, "go": runtime.Version(), "build": build, "rounds": c.Rounds, "duration_seconds": c.DurationSeconds, "flows": c.Flows, "timeout_ms": c.TimeoutMS, "endpoint_order": "alternating", "latency_window": campaign.SampleCapacity, "pid": c.PID}); err != nil {
		return err
	}
	events, err := os.OpenFile(filepath.Join(directory, "attempts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer events.Close()
	encoder := json.NewEncoder(events)
	clients := make([]*http.Client, len(c.Endpoints))
	udp := make([]udpSession, len(c.Endpoints))
	timeout := time.Duration(c.TimeoutMS) * time.Millisecond
	for i, e := range c.Endpoints {
		dialer := &net.Dialer{Timeout: timeout}
		dial := dialer.DialContext
		if e.SOCKS != "" {
			socks, err := proxy.SOCKS5("tcp", e.SOCKS, nil, dialer)
			if err != nil {
				return err
			}
			dial = socks.(proxy.ContextDialer).DialContext
		}
		transport := &http.Transport{DialContext: dial, DisableKeepAlives: true, ForceAttemptHTTP2: true, TLSHandshakeTimeout: timeout}
		clients[i] = &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		defer clients[i].CloseIdleConnections()
		if c.UDPEcho != "" {
			conn, err := openUDP(ctx, e, c.UDPEcho, timeout)
			udp[i] = udpSession{conn: conn, failed: err != nil}
			if conn != nil {
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
			}
		}
	}
	aggregates := map[string]*campaign.Aggregate{}
	wallTimes := map[string]time.Duration{}
	add := func(v campaign.Attempt) error {
		key := v.Endpoint + "/" + v.Target
		if aggregates[key] == nil {
			aggregates[key] = &campaign.Aggregate{}
		}
		aggregates[key].Add(v)
		return encoder.Encode(v)
	}
	start := time.Now()
	allFailedRounds, rounds := 0, 0
	resourceFailures := 0
	metricFailures := 0
	metricClient := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer metricClient.CloseIdleConnections()
	var peakRSS uint64
	for round := 0; c.Rounds == 0 || round < c.Rounds; round++ {
		if ctx.Err() != nil || (c.DurationSeconds > 0 && time.Since(start) >= time.Duration(c.DurationSeconds)*time.Second) {
			break
		}
		successes := 0
		for _, i := range campaign.Order(round, len(c.Endpoints)) {
			for _, target := range c.Targets {
				batchStart := time.Now()
				results := make(chan campaign.Attempt, c.Flows)
				var workers sync.WaitGroup
				for worker := 0; worker < c.Flows; worker++ {
					workers.Add(1)
					go func() {
						defer workers.Done()
						attempt, cancel := context.WithTimeout(ctx, timeout)
						defer cancel()
						results <- campaign.HTTP(attempt, clients[i], target)
					}()
				}
				workers.Wait()
				wallTimes[c.Endpoints[i].Name+"/"+target.Name] += time.Since(batchStart)
				close(results)
				for v := range results {
					v.Endpoint = c.Endpoints[i].Name
					v.Round = round
					if v.Success {
						successes++
					}
					if err = add(v); err != nil {
						return err
					}
				}
			}
			if c.UDPEcho != "" {
				probeStart := time.Now()
				v := campaign.Attempt{Target: "udp-echo", Kind: "udp", SetupFailure: true, Failure: "association"}
				if !udp[i].failed {
					v = probeUDP(udp[i].conn, uint64(round), timeout)
				}
				wallTimes[c.Endpoints[i].Name+"/udp-echo"] += time.Since(probeStart)
				v.Endpoint = c.Endpoints[i].Name
				v.Round = round
				if v.Success {
					successes++
				}
				if err = add(v); err != nil {
					return err
				}
			}
		}
		rounds++
		if successes == 0 {
			allFailedRounds++
		}
		if c.PID > 0 {
			sample, rss, err := processSample(c.PID)
			if err != nil {
				resourceFailures++
			}
			peakRSS = max(peakRSS, rss)
			if err = encoder.Encode(map[string]any{"kind": "resource", "round": round, "available": err == nil, "sample": sample}); err != nil {
				return err
			}
		}
		if c.MetricsURL != "" {
			sample := collectMetrics(ctx, metricClient, c.MetricsURL, os.Getenv(c.MetricsTokenEnv))
			sample.ElapsedMS = time.Since(start).Milliseconds()
			if !sample.Available {
				metricFailures++
			}
			if err = encoder.Encode(sample); err != nil {
				return err
			}
		}
		if c.IntervalMS > 0 {
			timer := time.NewTimer(time.Duration(c.IntervalMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	passed := len(aggregates) > 0 && resourceFailures == 0 && metricFailures == 0 && (c.MaxRSSBytes == 0 || peakRSS <= c.MaxRSSBytes)
	summaries := map[string]campaign.Summary{}
	for key, a := range aggregates {
		summary := a.Snapshot()
		summaries[key] = summary
		passed = passed && campaign.Gate(summary, c.MinSuccess, c.MaxP95MS, c.FinalSuccesses)
	}
	comparisons := map[string]bool{}
	if c.CompareTo != "" {
		targets := []string{}
		for _, target := range c.Targets {
			targets = append(targets, target.Name)
		}
		if c.UDPEcho != "" {
			targets = append(targets, "udp-echo")
		}
		for _, endpoint := range c.Endpoints {
			if endpoint.Name == c.CompareTo {
				continue
			}
			for _, target := range targets {
				a, b := c.CompareTo+"/"+target, endpoint.Name+"/"+target
				ok := campaign.Compare(summaries[a], summaries[b], wallTimes[a], wallTimes[b], c.MinCompletionRatio, c.MinGoodputRatio)
				comparisons[b] = ok
				passed = passed && ok
			}
		}
	}
	wallSeconds := map[string]float64{}
	for key, duration := range wallTimes {
		wallSeconds[key] = duration.Seconds()
	}
	if err = events.Close(); err != nil {
		return err
	}
	if err = exclusiveJSON(filepath.Join(directory, "summary.json"), map[string]any{"passed": passed, "rounds": rounds, "all_failed_rounds": allFailedRounds, "duration_seconds": time.Since(start).Seconds(), "resource_failures": resourceFailures, "metric_failures": metricFailures, "max_observed_rss_bytes": peakRSS, "summaries": summaries, "endpoint_wall_seconds": wallSeconds, "comparisons": comparisons, "interrupted": ctx.Err() != nil}); err != nil {
		return err
	}
	if !passed {
		return errors.New("campaign gates failed; see summary.json")
	}
	return nil
}

func main() {
	configFile := flag.String("config", "", "explicit campaign JSON configuration")
	output := flag.String("output", "", "new evidence directory")
	flag.Parse()
	if *configFile == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "-config and -output are required; no external endpoints are selected automatically")
		os.Exit(2)
	}
	data, err := os.ReadFile(*configFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read configuration")
		os.Exit(2)
	}
	var c config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			err = errors.New("trailing configuration")
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration JSON")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	sum := sha256.Sum256(data)
	if err = run(ctx, c, *output, hex.EncodeToString(sum[:])); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
