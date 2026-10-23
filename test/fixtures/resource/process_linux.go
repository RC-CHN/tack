//go:build linux

// Package resource samples an external process without forcing its GC or
// mixing the test controller's memory into the measured node's footprint.
package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Sample struct {
	At                time.Time `json:"at"`
	RSS               uint64    `json:"rss_bytes"`
	PeakRSS           uint64    `json:"peak_rss_bytes"`
	CPUTicks          uint64    `json:"cpu_ticks"`
	WriteBytes        uint64    `json:"storage_write_bytes"`
	WriteChars        uint64    `json:"write_syscall_bytes"`
	ReadChars         uint64    `json:"read_syscall_bytes"`
	FDs               int       `json:"file_descriptors"`
	Threads           uint64    `json:"threads"`
	VoluntarySwitches uint64    `json:"main_thread_voluntary_context_switches"`
}

func Process(pid int) (Sample, error) {
	root := filepath.Join("/proc", strconv.Itoa(pid))
	status, err := os.ReadFile(filepath.Join(root, "status"))
	if err != nil {
		return Sample{}, err
	}
	stat, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return Sample{}, err
	}
	io, err := os.ReadFile(filepath.Join(root, "io"))
	if err != nil {
		return Sample{}, err
	}
	fds, err := os.ReadDir(filepath.Join(root, "fd"))
	if err != nil {
		return Sample{}, err
	}
	value := func(data []byte, key string) (uint64, error) {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == key+":" {
				return strconv.ParseUint(f[1], 10, 64)
			}
		}
		return 0, fmt.Errorf("missing process metric %s", key)
	}
	s := Sample{At: time.Now().UTC(), FDs: len(fds)}
	for _, field := range []struct {
		data []byte
		key  string
		out  *uint64
	}{{status, "VmRSS", &s.RSS}, {status, "VmHWM", &s.PeakRSS}, {status, "Threads", &s.Threads}, {status, "voluntary_ctxt_switches", &s.VoluntarySwitches}, {io, "write_bytes", &s.WriteBytes}, {io, "wchar", &s.WriteChars}, {io, "rchar", &s.ReadChars}} {
		*field.out, err = value(field.data, field.key)
		if err != nil {
			return Sample{}, err
		}
	}
	s.RSS *= 1024
	s.PeakRSS *= 1024
	// comm may contain spaces and parentheses; fields after its final ')' start
	// at field 3. utime/stime are fields 14/15. Include this process, not children.
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 {
		return Sample{}, fmt.Errorf("invalid proc stat")
	}
	f := strings.Fields(string(stat)[end+2:])
	if len(f) < 13 {
		return Sample{}, fmt.Errorf("short proc stat")
	}
	for _, idx := range []int{11, 12} {
		n, e := strconv.ParseUint(f[idx], 10, 64)
		if e != nil {
			return Sample{}, e
		}
		s.CPUTicks += n
	}
	return s, nil
}
