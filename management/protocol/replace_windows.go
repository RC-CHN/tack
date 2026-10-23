package protocol

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// ReplaceFile tolerates readers/scanners briefly opening a state file without
// FILE_SHARE_DELETE. Retries never delete the destination or report success
// before replacement, and permanent access failures remain bounded to one second.
// Callers serialize mutations and sync the containing directory separately.
func ReplaceFile(from, to string) error {
	deadline := time.Now().Add(time.Second)
	delay := 10 * time.Millisecond
	for {
		err := os.Rename(from, to)
		if err == nil || (!errors.Is(err, syscall.ERROR_ACCESS_DENIED) &&
			!errors.Is(err, syscall.Errno(32)) && !errors.Is(err, syscall.Errno(33))) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
			if delay > 200*time.Millisecond {
				delay = 200 * time.Millisecond
			}
		}
	}
}
