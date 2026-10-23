//go:build windows

package linklab

import (
	"errors"
	"syscall"
)

func datagramError(err error) bool {
	// Winsock: reset, refused, host unreachable, network unreachable. These
	// describe a datagram delivery failure, not closure of the local socket.
	for _, code := range []syscall.Errno{10054, 10061, 10065, 10051} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
