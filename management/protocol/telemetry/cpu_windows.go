package telemetry

import "syscall"

func cpuTime() (int64, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, false
	}
	ticks := func(v syscall.Filetime) uint64 { return uint64(v.HighDateTime)<<32 | uint64(v.LowDateTime) }
	return int64((ticks(kernel) + ticks(user)) / 10000), true
}
