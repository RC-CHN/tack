// bench-origin is an explicitly started, bounded HTTP measurement endpoint.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"time"
)

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func handler(limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/bytes":
			n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
			if err != nil || n < 0 || n > limit {
				http.Error(w, "invalid length", 400)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			w.Header().Set("Content-Type", "application/octet-stream")
			io.CopyN(w, zeroReader{}, n)
		case r.Method == "POST" && r.URL.Path == "/upload":
			if r.ContentLength < 0 {
				http.Error(w, "Content-Length required", 411)
				return
			}
			if r.ContentLength > limit {
				w.Header().Set("Connection", "close")
				http.Error(w, "body exceeds limit", 413)
				return
			}
			h := sha256.New()
			n, err := io.CopyN(h, r.Body, r.ContentLength)
			if err != nil {
				http.Error(w, "incomplete upload", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"bytes": n, "sha256": hex.EncodeToString(h.Sum(nil))})
		default:
			http.Error(w, "unknown measurement route", 404)
		}
	})
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "explicit bind address")
	limit := flag.Int64("max-bytes", 64<<20, "maximum request/download bytes")
	requests := flag.Int64("requests", 1, "exit after this many requests")
	duration := flag.Duration("duration", time.Minute, "maximum listener lifetime")
	flag.Parse()
	if *limit < 1 || *limit > 1<<30 || *requests < 1 || *requests > 1000000 || *duration <= 0 || *duration > 24*time.Hour {
		fmt.Fprintln(os.Stderr, "invalid endpoint bounds")
		os.Exit(2)
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(l.Addr().String())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, *duration)
	defer stop()
	done := make(chan struct{}, 1)
	var count atomic.Int64
	inner := handler(*limit)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) > *requests {
			http.Error(w, "measurement finished", 503)
			return
		}
		defer func() {
			if count.Load() >= *requests {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		}()
		inner.ServeHTTP(w, r)
	})}
	go server.Serve(l)
	select {
	case <-ctx.Done():
	case <-done:
	}
	shutdown, finish := context.WithTimeout(context.Background(), time.Second)
	defer finish()
	server.Shutdown(shutdown)
	server.Close()
}
