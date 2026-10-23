package runner

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var createJob = kernel.NewProc("CreateJobObjectW")
var setJob = kernel.NewProc("SetInformationJobObject")
var assignJob = kernel.NewProc("AssignProcessToJobObject")
var terminateJob = kernel.NewProc("TerminateJobObject")
var resumeThread = kernel.NewProc("ResumeThread")

// Create the child suspended, attach it to a non-inheritable kill-on-close Job,
// and only then resume it. No task code runs outside the Job.
func runProcess(ctx context.Context, cmd *exec.Cmd, mode string) error {
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(job))
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION: 112 bytes on Win32, 144 on Win64.
	size := 144
	if unsafe.Sizeof(uintptr(0)) == 4 {
		size = 112
	}
	limits := make([]byte, size)
	binary.LittleEndian.PutUint32(limits[16:], 0x2000)
	if ok, _, err := setJob.Call(job, 9, uintptr(unsafe.Pointer(&limits[0])), uintptr(size)); ok == 0 {
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer errR.Close()
	defer errW.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	app, err := syscall.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return err
	}
	var escaped []string
	for _, arg := range cmd.Args {
		escaped = append(escaped, syscall.EscapeArg(arg))
	}
	line := strings.Join(escaped, " ")
	if mode == "cmd" { // CMD's /s /c grammar is different from CommandLineToArgvW.
		file := cmd.Args[len(cmd.Args)-1]
		if strings.ContainsAny(file, "\"\r\n%") {
			return fmt.Errorf("CMD workspace path contains unsupported characters")
		}
		line = syscall.EscapeArg(cmd.Path) + ` /d /s /c ""` + file + `""`
	}
	commandLine, err := syscall.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	dir, err := syscall.UTF16PtrFromString(cmd.Dir)
	if err != nil {
		return err
	}
	var environment []uint16
	entries := cmd.Environ()
	sort.Slice(entries, func(i, j int) bool { return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j]) })
	for _, entry := range entries {
		v, err := syscall.UTF16FromString(entry)
		if err != nil {
			return err
		}
		environment = append(environment, v...)
	}
	environment = append(environment, 0)
	if len(environment) == 1 {
		environment = append(environment, 0)
	}
	startup := syscall.StartupInfo{Flags: syscall.STARTF_USESTDHANDLES, StdInput: syscall.Handle(null.Fd()), StdOutput: syscall.Handle(outW.Fd()), StdErr: syscall.Handle(errW.Fd())}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var pi syscall.ProcessInformation
	// Coordinate inheritable standard handles with other os/exec launches.
	syscall.ForkLock.Lock()
	for _, h := range []syscall.Handle{startup.StdInput, startup.StdOutput, startup.StdErr} {
		if err = syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
			break
		}
	}
	if err == nil {
		err = syscall.CreateProcess(app, commandLine, nil, nil, true, 0x4|syscall.CREATE_UNICODE_ENVIRONMENT, &environment[0], dir, &startup, &pi)
	}
	outW.Close()
	errW.Close()
	null.Close()
	syscall.ForkLock.Unlock()
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(pi.Process)
	defer syscall.CloseHandle(pi.Thread)
	if ok, _, err := assignJob.Call(job, uintptr(pi.Process)); ok == 0 {
		syscall.TerminateProcess(pi.Process, 1)
		syscall.WaitForSingleObject(pi.Process, syscall.INFINITE)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		terminateJob.Call(job, 1)
		return err
	}
	cmd.Process = proc
	var copying sync.WaitGroup
	copying.Add(2)
	go func() { defer copying.Done(); io.Copy(cmd.Stdout, outR) }()
	go func() { defer copying.Done(); io.Copy(cmd.Stderr, errR) }()
	if result, _, err := resumeThread.Call(uintptr(pi.Thread)); result == 0xffffffff {
		terminateJob.Call(job, 1)
		proc.Wait()
		copying.Wait()
		return fmt.Errorf("ResumeThread: %w", err)
	}
	done, watcher := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watcher)
		select {
		case <-ctx.Done():
			terminateJob.Call(job, 1)
		case <-done:
		}
	}()
	cmd.ProcessState, err = proc.Wait()
	close(done)
	<-watcher
	terminateJob.Call(job, 1) // close descendant-held stdout/stderr before draining
	copying.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && !cmd.ProcessState.Success() {
		return &exec.ExitError{ProcessState: cmd.ProcessState}
	}
	return err
}
