package linklab

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestDatagramErrorsAreDistinctFromSocketLifetime(t *testing.T) {
	for _, err := range []error{nil, net.ErrClosed, context.Canceled, io.EOF, syscall.EBADF, syscall.EPERM, errors.New("refused text is not a kernel code")} {
		if datagramError(err) {
			t.Fatal("fatal/unknown error tolerated", err)
		}
	}
	codes := []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH}
	if runtime.GOOS == "windows" {
		codes = []syscall.Errno{10054, 10061, 10065, 10051}
	}
	for _, code := range codes {
		err := &net.OpError{Op: "read", Net: "udp", Err: os.NewSyscallError("recvfrom", code)}
		if !datagramError(err) {
			t.Fatal("wrapped datagram failure not recognized", code)
		}
	}
}
