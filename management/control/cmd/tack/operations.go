package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	p "tack.local/management/protocol"
)

type commandError struct {
	code    int
	message string
}

func (e *commandError) Error() string { return e.message }
func exitCode(err error) int {
	var e *commandError
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

type apiClient struct {
	client        *http.Client
	origin, token string
}

func (c apiClient) request(ctx context.Context, method, path string, input any) ([]byte, error) {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16*p.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 16*p.MaxBody {
		return nil, errors.New("controller response exceeds size limit")
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("controller returned %d: %s", resp.StatusCode, b)
	}
	return b, nil
}

func run(args []string) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "-version" || args[0] == "--version") {
		fmt.Println(p.BuildVersion)
		return nil
	}
	if len(args) == 1 && args[0] == "request-id" {
		id, err := p.NewRequestID()
		if err == nil {
			_, err = fmt.Fprintln(os.Stdout, id)
		}
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: tack provision|request-id|nodes|metrics|tasks|submit|get|wait|cancel|operators|operator-set|operator-revoke [options]")
	}
	if args[0] == "provision" {
		return provision(args[1:])
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	server := f.String("server", "", "controller HTTPS origin")
	configPath := f.String("config", "", "local controller config supplying administrator token")
	credential := f.String("credential", "", "restricted credential JSON containing token (instead of controller config)")
	ca := f.String("ca", "", "private CA file")
	dev := f.Bool("allow-insecure-loopback", false, "development HTTP on literal loopback")
	id := f.String("id", "", "task or operator ID")
	file := f.String("file", "", "task or operator JSON file")
	limit := f.Int("limit", 100, "task page size (1..200)")
	after := f.String("after", "", "task page cursor")
	node := f.String("node", "", "task node filter")
	state := f.String("state", "", "task state filter")
	caller := f.String("caller-id", "", "caller correlation ID; filter for tasks, assignment for submit")
	owner := f.String("owner", "", "task owner filter")
	since := f.String("since", "", "inclusive created-at lower bound, RFC3339")
	until := f.String("until", "", "inclusive created-at upper bound, RFC3339")
	requestID := f.String("request-id", "", "submission identity; reuse for every retry")
	startBefore := f.String("start-before", "", "latest allowed command start, RFC3339")
	format := f.String("format", "", "text, json (decoded output), or json-raw (protocol/base64); get/wait default to text")
	stdoutFile := f.String("stdout-file", "", "save exact stdout bytes when output is complete")
	stderrFile := f.String("stderr-file", "", "save exact stderr bytes when output is complete")
	timeout := f.Duration("timeout", 5*time.Minute, "wait budget; expiration never cancels the remote task")
	poll := f.Duration("poll", time.Second, "wait polling interval (100ms..30s)")
	check := f.Bool("check", false, "wait: exit 3 unless remote execution succeeded; cleanup warnings remain separate")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *check && args[0] != "wait" {
		return errors.New("--check requires wait; get always retains query semantics")
	}
	if *format != "" && *format != "text" && *format != "json" && *format != "json-raw" {
		return errors.New("format must be text, json or json-raw")
	}
	if *timeout <= 0 || *timeout > 24*time.Hour || *poll < 100*time.Millisecond || *poll > 30*time.Second {
		return errors.New("timeout must be 0..24h (exclusive zero); poll must be 100ms..30s")
	}
	if *credential != "" && *configPath != "" {
		return errors.New("use either credential or config")
	}
	var token string
	if *credential != "" {
		var c p.ClientCredential
		if err := p.ReadJSON(*credential, &c); err != nil {
			return err
		}
		token = c.Token
	} else {
		var c p.ControlConfig
		if err := p.ReadJSON(*configPath, &c); err != nil {
			return err
		}
		token = c.AdminToken
	}
	if len(token) < 32 {
		return errors.New("valid credential required")
	}
	client, origin, err := p.HTTPClient(*server, *ca, *dev)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	api := apiClient{client: client, origin: origin, token: token}
	method, path := "GET", ""
	var input any
	taskOutput := false
	switch args[0] {
	case "nodes":
		path = "/v1/nodes"
	case "metrics":
		path = "/v1/metrics"
	case "tasks":
		if *limit < 1 || *limit > 200 || (*after != "" && !p.ValidID(*after)) {
			return errors.New("invalid task page limit/cursor")
		}
		path = "/v2/tasks?" + url.Values{"limit": {strconv.Itoa(*limit)}, "after": {*after}, "node": {*node}, "state": {*state}, "caller_id": {*caller}, "owner": {*owner}, "since": {*since}, "until": {*until}}.Encode()
	case "get", "cancel", "wait":
		if !p.ValidID(*id) {
			return errors.New("valid -id required")
		}
		path = "/v1/tasks/" + *id
		taskOutput = true
		if args[0] == "cancel" {
			method = "POST"
			path += "/cancel"
		}
	case "submit":
		task, err := prepareSubmission(*file, *requestID, *caller, *startBefore)
		if err != nil {
			return err
		}
		input = task
		method, path = "POST", "/v1/tasks"
		taskOutput = true
	case "operators":
		path = "/v1/operators"
	case "operator-set":
		var op p.Operator
		if err := p.ReadJSON(*file, &op); err != nil {
			return err
		}
		input = op
		method, path = "POST", "/v1/operators"
	case "operator-revoke":
		if !p.ValidID(*id) {
			return errors.New("valid operator -id required")
		}
		method, path = "DELETE", "/v1/operators/"+*id
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if (*stdoutFile != "" || *stderrFile != "") && !taskOutput {
		return errors.New("output files require a task command")
	}
	if *format == "" {
		*format = "json-raw"
		if args[0] == "get" || args[0] == "wait" {
			*format = "text"
		}
	}
	var b []byte
	if args[0] == "wait" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		b, err = waitTask(ctx, api, path, *poll)
	} else {
		b, err = api.request(context.Background(), method, path, input)
	}
	if len(b) > 0 {
		if taskOutput {
			if err != nil {
				*stdoutFile, *stderrFile = "", ""
			}
			if e := renderTask(os.Stdout, b, *format, *stdoutFile, *stderrFile); e != nil {
				return e
			}
		} else {
			if _, e := os.Stdout.Write(b); e != nil {
				return e
			}
		}
	}
	if err != nil {
		return err
	}
	if *check {
		var t p.Task
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		if t.State != "succeeded" {
			return &commandError{3, "remote task ended in state " + t.State}
		}
	}
	return nil
}

// Save an automatically generated identity BEFORE making an HTTP request. A
// lost response, process crash, or concurrent invocation can then safely retry
// the same file. Explicit identity/metadata flags are also persisted for retry.
func prepareSubmission(path, id, caller, before string) (p.CreateTask, error) {
	var task p.CreateTask
	unlock, err := p.Lock(path + ".lock")
	if err != nil {
		return task, err
	}
	defer unlock()
	if err := p.ReadJSON(path, &task); err != nil {
		return task, err
	}
	task.Command.Defaults()
	if err := task.Command.Validate(); err != nil {
		return task, err
	}
	if task.RequestID != "" && id != "" && task.RequestID != id {
		return task, errors.New("request-id conflicts with the identity already saved in the task file")
	}
	if task.RequestID == "" {
		task.RequestID = id
	}
	if task.RequestID == "" {
		task.RequestID, err = p.NewRequestID()
		if err != nil {
			return task, err
		}
	}
	if _, err = p.RequestTime(task.RequestID); err != nil {
		return task, err
	}
	if caller != "" {
		task.CallerID = caller
	}
	if before != "" {
		t, e := time.Parse(time.RFC3339Nano, before)
		if e != nil {
			return task, e
		}
		task.StartBefore = &t
	}
	if err := p.WriteJSON(path, task); err != nil {
		return task, fmt.Errorf("cannot persist submission identity; request was not sent: %w", err)
	}
	return task, nil
}

func waitTask(ctx context.Context, api apiClient, path string, interval time.Duration) ([]byte, error) {
	var last []byte
	for {
		b, err := api.request(ctx, "GET", path, nil)
		if ctx.Err() != nil {
			return last, &commandError{4, "wait_timeout: stopped waiting; remote task was not canceled"}
		}
		if err != nil {
			return last, err
		}
		var t p.Task
		if err := json.Unmarshal(b, &t); err != nil {
			return last, err
		}
		last = b
		if p.Terminal(t.State) && !t.ResultPending {
			return b, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, &commandError{4, "wait_timeout: stopped waiting; remote task was not canceled"}
		case <-timer.C:
		}
	}
}

type decodedResult struct {
	*p.Result
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTextLossy bool   `json:"stdout_text_lossy,omitempty"`
	StderrTextLossy bool   `json:"stderr_text_lossy,omitempty"`
}

func renderTask(w io.Writer, raw []byte, format, stdoutPath, stderrPath string) error {
	var t p.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	if stdoutPath != "" || stderrPath != "" {
		if t.ResultPending || t.Result == nil {
			return errors.New("output_not_ready: no output files written; wait for result upload to complete")
		}
		a, _ := filepath.Abs(stdoutPath)
		b, _ := filepath.Abs(stderrPath)
		if stdoutPath != "" && stderrPath != "" && a == b {
			return errors.New("stdout and stderr files must differ")
		}
		for path, data := range map[string][]byte{stdoutPath: t.Result.Stdout, stderrPath: t.Result.Stderr} {
			if path != "" {
				if err := p.WriteFile(path, data); err != nil {
					return err
				}
			}
		}
	}
	switch format {
	case "json-raw":
		_, err := w.Write(raw)
		return err
	case "json":
		out := struct {
			p.Task
			Result *decodedResult `json:"result,omitempty"`
		}{Task: t}
		if t.Result != nil {
			out.Result = &decodedResult{Result: t.Result, Stdout: string(t.Result.Stdout), Stderr: string(t.Result.Stderr), StdoutTextLossy: !utf8.Valid(t.Result.Stdout), StderrTextLossy: !utf8.Valid(t.Result.Stderr)}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "text":
		if _, err := fmt.Fprintf(w, "task: %s\nnode: %s\nstate: %s\noutput_pending: %t\n", t.ID, t.NodeID, t.State, t.ResultPending); err != nil {
			return err
		}
		if t.Transfer != nil && t.Transfer.ErrorCode != "" {
			if _, err := fmt.Fprintf(w, "delivery_error: %s (%s): %s\n", t.Transfer.ErrorCode, t.Transfer.Phase, t.Transfer.Error); err != nil {
				return err
			}
		}
		if r := t.Result; r != nil {
			exit := "not available"
			if r.ExitCode != nil {
				exit = strconv.Itoa(*r.ExitCode)
			}
			if _, err := fmt.Fprintf(w, "exit_code: %s\nphase: %s\nerror_code: %s\nstdout_truncated: %t\nstderr_truncated: %t\nmetadata_truncated: %t\n", exit, r.Phase, r.ErrorCode, r.StdoutTruncated, r.StderrTruncated, r.MetadataTruncated); err != nil {
				return err
			}
			if r.Error != "" {
				if _, err := fmt.Fprintf(w, "error: %s\n", r.Error); err != nil {
					return err
				}
			}
			if r.Cleanup != nil && r.Cleanup.ErrorCode != "" {
				if _, err := fmt.Fprintf(w, "cleanup_error: %s: %s\ncleanup_path: %s\n", r.Cleanup.ErrorCode, r.Cleanup.Error, r.Cleanup.Path); err != nil {
					return err
				}
			}
			for _, stream := range []struct {
				name string
				data []byte
			}{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
				if _, err := fmt.Fprintf(w, "--- %s ---\n", stream.name); err != nil {
					return err
				}
				if _, err := w.Write(stream.data); err != nil {
					return err
				}
				if len(stream.data) > 0 && stream.data[len(stream.data)-1] != '\n' {
					if _, err := fmt.Fprintln(w); err != nil {
						return err
					}
				}
			}
		}
		return nil
	default:
		return errors.New("invalid output format")
	}
}
