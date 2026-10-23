// Package rotatelog provides a small, synchronous, byte-bounded diagnostic log.
// Task results and execution journals must never be written through this sink.
package rotatelog

import (
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
)

type Writer struct {
	mu    sync.Mutex
	path  string
	max   int64
	files int
	f     *os.File
	size  int64
}

func Open(path string, max int64, files int) (*Writer, error) {
	if path == "" || max < 1024 || max > 1024*1024*1024 || files < 1 || files > 32 {
		return nil, errors.New("log limits require 1024..1073741824 bytes and 1..32 files")
	}
	w := &Writer{path: path, max: max, files: files}
	for i := 1; i < 32; i++ {
		name := path + "." + strconv.Itoa(i)
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if i >= files || !info.Mode().IsRegular() || info.Size() > max {
			if err := os.Remove(name); err != nil {
				return nil, err
			}
		}
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	if w.size > max {
		if err := w.f.Truncate(0); err != nil {
			w.Close()
			return nil, err
		}
		w.size = 0
	}
	return w, nil
}

func (w *Writer) open() error {
	if info, err := os.Lstat(w.path); err == nil && !info.Mode().IsRegular() {
		return errors.New("log path must be a regular file")
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

func (w *Writer) rotate() error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}
	// Fixed names bound file count across restarts. Never follow backup links.
	last := w.path
	if w.files > 1 {
		last += "." + strconv.Itoa(w.files-1)
	}
	if err := os.Remove(last); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := w.files - 2; i >= 0; i-- {
		src := w.path
		if i > 0 {
			src += "." + strconv.Itoa(i)
		}
		if err := os.Rename(src, w.path+"."+strconv.Itoa(i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return w.open()
}

func (w *Writer) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	written := 0
	for len(b) > 0 {
		if w.size >= w.max {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}
		n := len(b)
		if int64(n) > w.max-w.size {
			n = int(w.max - w.size)
		}
		m, err := w.f.Write(b[:n])
		written += m
		w.size += int64(m)
		b = b[m:]
		if err != nil {
			return written, err
		}
		if m != n {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

type Options struct {
	path   *string
	max    *int64
	files  *int
	stderr *bool
}

func Flags(f *flag.FlagSet) Options {
	return Options{path: f.String("log-file", "", "enable rotating diagnostic logs at this path (default disabled)"), max: f.Int64("log-max-bytes", 4*1024*1024, "maximum bytes per diagnostic log file"), files: f.Int("log-files", 4, "total diagnostic log files, including current"), stderr: f.Bool("log-stderr", false, "enable diagnostic logging to stderr")}
}

func (o Options) Enabled() bool { return *o.path != "" || *o.stderr }

func (o Options) Install() (func(), error) {
	if *o.path != "" && *o.stderr {
		return nil, errors.New("choose -log-file or -log-stderr")
	}
	if *o.path == "" {
		previous := log.Writer()
		if *o.stderr {
			log.SetOutput(os.Stderr)
		} else {
			log.SetOutput(io.Discard)
		}
		return func() { log.SetOutput(previous) }, nil
	}
	w, err := Open(*o.path, *o.max, *o.files)
	if err != nil {
		return nil, err
	}
	previous := log.Writer()
	log.SetOutput(w)
	return func() { log.SetOutput(previous); w.Close() }, nil
}
