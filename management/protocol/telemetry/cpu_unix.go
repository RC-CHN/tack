//go:build linux || darwin || freebsd || openbsd || netbsd

package telemetry

import "syscall"

func cpuTime() (int64, bool) {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0, false
	}
	return (int64(usage.Utime.Sec)+int64(usage.Stime.Sec))*1000 + (int64(usage.Utime.Usec)+int64(usage.Stime.Usec))/1000, true
}
