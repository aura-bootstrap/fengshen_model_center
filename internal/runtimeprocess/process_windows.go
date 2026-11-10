//go:build windows

package runtimeprocess

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var (
	createJob        = kernel32.NewProc("CreateJobObjectW")
	setJobInfo       = kernel32.NewProc("SetInformationJobObject")
	inJob            = kernel32.NewProc("IsProcessInJob")
	initAttributes   = kernel32.NewProc("InitializeProcThreadAttributeList")
	updateAttributes = kernel32.NewProc("UpdateProcThreadAttribute")
	deleteAttributes = kernel32.NewProc("DeleteProcThreadAttributeList")
	localAlloc       = kernel32.NewProc("LocalAlloc")
	localFree        = kernel32.NewProc("LocalFree")
	resumeThread     = kernel32.NewProc("ResumeThread")
	queryImage       = kernel32.NewProc("QueryFullProcessImageNameW")
	finalPath        = kernel32.NewProc("GetFinalPathNameByHandleW")
	getPID           = kernel32.NewProc("GetProcessId")
)

const (
	createSuspended     = 0x4
	extendedStartup     = 0x80000
	createNoWindow      = 0x08000000
	attributeHandleList = 0x20002
	attributeJobList    = 0x2000d
	jobKillOnClose      = 0x2000
)

type startupInfoEx struct {
	syscall.StartupInfo
	Attributes uintptr
}
type nativeProcess struct{ process, job syscall.Handle }

func winError(name string, err error) error {
	if err == nil || err == syscall.Errno(0) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("runtimeprocess: %s: %w", name, err)
}

// openImage disallows write/delete sharing and rejects a final reparse point.
// The handle remains open across hashing AND CreateProcess+identity validation,
// pinning the file against rename/replacement and ordinary concurrent writes.
func openImage(path string) (*os.File, fileIdentity, string, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fileIdentity{}, "", ErrIdentity
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0x00200000, 0)
	if err != nil {
		return nil, fileIdentity{}, "", fmt.Errorf("runtimeprocess: open image: %w", err)
	}
	f := os.NewFile(uintptr(h), path)
	fail := func(err error) (*os.File, fileIdentity, string, error) {
		f.Close()
		return nil, fileIdentity{}, "", err
	}
	var info syscall.ByHandleFileInformation
	if err = syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fail(err)
	}
	if info.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fail(ErrIdentity)
	}
	buf := make([]uint16, 32768)
	n, _, e := finalPath.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if n == 0 || n >= uintptr(len(buf)) {
		return fail(winError("GetFinalPathNameByHandle", e))
	}
	canonical := syscall.UTF16ToString(buf[:n])
	// Use a DOS/UNC path accepted by both the Win32 loader and Go's path helpers.
	if strings.HasPrefix(canonical, `\\?\UNC\`) {
		canonical = `\\` + canonical[8:]
	} else {
		canonical = strings.TrimPrefix(canonical, `\\?\`)
	}
	id := fileIdentity{Volume: info.VolumeSerialNumber, IndexHigh: info.FileIndexHigh, IndexLow: info.FileIndexLow, Size: uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow), Modified: filetime(info.LastWriteTime)}
	return f, id, canonical, nil
}
func filetime(t syscall.Filetime) uint64 { return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime) }

// minimalEnvironment does not read the parent's environment, including its
// SystemRoot/PATH/TEMP values. Only OS-reported local system paths are used.
func minimalEnvironment(extra []string) ([]uint16, error) {
	queryDirectory := func(name string) (string, error) {
		buf := make([]uint16, 32768)
		n, _, err := kernel32.NewProc(name).Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 || n >= uintptr(len(buf)) {
			return "", winError(name, err)
		}
		path := syscall.UTF16ToString(buf[:n])
		if !filepath.IsAbs(path) || strings.ContainsAny(path, ";\x00") {
			return "", errors.New("runtimeprocess: invalid system directory")
		}
		return path, nil
	}
	windows, err := queryDirectory("GetWindowsDirectoryW")
	if err != nil {
		return nil, err
	}
	system, err := queryDirectory("GetSystemDirectoryW")
	if err != nil {
		return nil, err
	}
	// Avoid GetTempPath/os.TempDir: both can consume untrusted parent variables.
	temp := filepath.Join(windows, "Temp")
	entries := []string{"PATH=" + system + ";" + windows, "SystemRoot=" + windows, "TEMP=" + temp, "TMP=" + temp, "windir=" + windows}
	if len(extra) != 0 {
		entries = append(append([]string(nil), extra...), entries...)
	}
	// Entries are sorted case-insensitively; every entry and the block terminate
	// with NUL. Keep the explicit block alive until CreateProcess returns.
	return utf16.Encode([]rune(strings.Join(entries, "\x00") + "\x00\x00")), nil
}

func launch(ctx context.Context, b VerifiedBinary, o Options, extraEnv []string) (*nativeProcess, *os.File, *os.File, *os.File, Identity, error) {
	var id Identity
	environment, err := minimalEnvironment(extraEnv)
	if err != nil {
		return nil, nil, nil, nil, id, err
	}
	// Each root gets a new unnamed Job. NULL security attributes means the Job
	// handle is non-inheritable; no breakaway flag is enabled.
	j, _, e := createJob.Call(0, 0)
	if j == 0 {
		return nil, nil, nil, nil, id, winError("CreateJobObject", e)
	}
	job := syscall.Handle(j)
	ownedJob := true
	defer func() {
		if ownedJob {
			syscall.CloseHandle(job)
		}
	}()
	// Windows uses 8-byte structure alignment on x86 as well; Go/386 does not.
	// Only LimitFlags (offset 16 on both ABIs) is nonzero. Use the exact native
	// buffer length rather than relying on Go struct padding.
	var limits [144]byte
	limitSize := uintptr(144)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		limitSize = 112
	}
	binary.LittleEndian.PutUint32(limits[16:20], jobKillOnClose)
	ok, _, e := setJobInfo.Call(j, 9, uintptr(unsafe.Pointer(&limits[0])), limitSize)
	if ok == 0 {
		return nil, nil, nil, nil, id, winError("SetInformationJobObject", e)
	}

	// Only these three duplicated child pipe handles are inheritable. The handle
	// allowlist excludes every other center handle, notably this and other Jobs.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, id, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, nil, nil, id, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, nil, nil, id, err
	}
	success := false
	defer func() {
		inR.Close()
		outW.Close()
		errW.Close()
		if !success {
			inW.Close()
			outR.Close()
			errR.Close()
		}
	}()
	current := syscall.Handle(^uintptr(0))
	handles := [3]syscall.Handle{}
	defer func() {
		for _, h := range handles {
			if h != 0 {
				syscall.CloseHandle(h)
			}
		}
	}()
	for i, f := range []*os.File{inR, outW, errW} {
		if err = syscall.DuplicateHandle(current, syscall.Handle(f.Fd()), current, &handles[i], 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, nil, nil, nil, id, err
		}
	}
	var size uintptr
	initAttributes.Call(0, 2, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, nil, nil, nil, id, ErrUnsupported
	}
	attrs, _, e := localAlloc.Call(0, size)
	if attrs == 0 {
		return nil, nil, nil, nil, id, winError("LocalAlloc", e)
	}
	defer localFree.Call(attrs)
	ok, _, e = initAttributes.Call(attrs, 2, 0, uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return nil, nil, nil, nil, id, winError("InitializeProcThreadAttributeList", e)
	}
	defer deleteAttributes.Call(attrs)
	jobs := [1]syscall.Handle{job}
	ok, _, e = updateAttributes.Call(attrs, 0, attributeJobList, uintptr(unsafe.Pointer(&jobs[0])), unsafe.Sizeof(jobs), 0, 0)
	if ok == 0 {
		return nil, nil, nil, nil, id, winError("atomic job-list unsupported", e)
	}
	ok, _, e = updateAttributes.Call(attrs, 0, attributeHandleList, uintptr(unsafe.Pointer(&handles[0])), unsafe.Sizeof(handles), 0, 0)
	if ok == 0 {
		return nil, nil, nil, nil, id, winError("handle allowlist", e)
	}
	app, err := syscall.UTF16PtrFromString(b.path)
	if err != nil {
		return nil, nil, nil, nil, id, err
	}
	args := []string{syscall.EscapeArg(b.path)}
	for _, a := range o.Args {
		args = append(args, syscall.EscapeArg(a))
	}
	cmd, err := syscall.UTF16PtrFromString(strings.Join(args, " "))
	if err != nil {
		return nil, nil, nil, nil, id, err
	}
	dir, err := syscall.UTF16PtrFromString(filepath.Dir(b.path))
	if err != nil {
		return nil, nil, nil, nil, id, err
	}
	si := startupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput = handles[0]
	si.StdOutput = handles[1]
	si.StdErr = handles[2]
	si.Attributes = attrs
	var pi syscall.ProcessInformation
	if err = ctx.Err(); err != nil {
		return nil, nil, nil, nil, id, err
	}
	err = syscall.CreateProcess(app, cmd, nil, nil, true, createSuspended|extendedStartup|createNoWindow|syscall.CREATE_UNICODE_ENVIRONMENT, &environment[0], dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(environment)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(jobs)
	if err != nil {
		return nil, nil, nil, nil, id, fmt.Errorf("runtimeprocess: CreateProcess in owned Job: %w", err)
	}
	defer syscall.CloseHandle(pi.Thread)
	native := &nativeProcess{process: pi.Process, job: job}
	ownedJob = false
	fail := func(cause error) (*nativeProcess, *os.File, *os.File, *os.File, Identity, error) {
		cleanup := native.closeJob()
		// A second, handle-based kill also covers any failed job setup/validation.
		_ = syscall.TerminateProcess(pi.Process, 1)
		waitErr := native.wait()
		closeErr := native.closeProcess()
		// Nonzero termination status is expected here, not a cleanup error.
		var exit *ExitError
		if errors.As(waitErr, &exit) {
			waitErr = nil
		}
		return nil, nil, nil, nil, Identity{}, errors.Join(cause, cleanup, waitErr, closeErr)
	}
	id, err = native.readIdentity(b)
	if err != nil {
		return fail(err)
	}
	if id.PID != pi.ProcessId {
		return fail(ErrIdentity)
	}
	if err = native.check(id, b); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	count, _, e := resumeThread.Call(uintptr(pi.Thread))
	if count != 1 {
		return fail(winError("ResumeThread (expected suspend count 1)", e))
	}
	success = true
	return native, inW, outR, errR, id, nil
}
func (n *nativeProcess) readIdentity(b VerifiedBinary) (Identity, error) {
	var id Identity
	pid, _, e := getPID.Call(uintptr(n.process))
	if pid == 0 {
		return id, winError("GetProcessId", e)
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(n.process, &creation, &exit, &kernel, &user); err != nil {
		return id, err
	}
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	ok, _, e := queryImage.Call(uintptr(n.process), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return id, winError("QueryFullProcessImageName", e)
	}
	path := syscall.UTF16ToString(buf[:size])
	f, file, canonical, err := openImage(path)
	if err != nil {
		return id, err
	}
	defer f.Close()
	if file != b.file {
		return id, ErrIdentity
	}
	id = Identity{PID: uint32(pid), CreationTime: filetime(creation), ImagePath: canonical, SHA256: b.digest, Component: b.component, Binding: b.binding}
	if id.CreationTime == 0 {
		return Identity{}, ErrIdentity
	}
	return id, nil
}
func (n *nativeProcess) check(id Identity, b VerifiedBinary) error {
	state, err := syscall.WaitForSingleObject(n.process, 0)
	if err != nil {
		return err
	}
	if state != syscall.WAIT_TIMEOUT {
		return ErrIdentity
	}
	var member int32
	ok, _, e := inJob.Call(uintptr(n.process), uintptr(n.job), uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return winError("IsProcessInJob", e)
	}
	if member == 0 {
		return ErrIdentity
	}
	actual, err := n.readIdentity(b)
	if err != nil {
		return err
	}
	if actual != id {
		return ErrIdentity
	}
	return nil
}
func (n *nativeProcess) closeJob() error {
	if n.job == 0 {
		return nil
	}
	err := syscall.CloseHandle(n.job)
	if err == nil {
		n.job = 0
	}
	return err
}
func (n *nativeProcess) closeProcess() error {
	err := syscall.CloseHandle(n.process)
	n.process = 0
	return err
}

func (n *nativeProcess) wait() error {
	status, err := syscall.WaitForSingleObject(n.process, syscall.INFINITE)
	if err != nil {
		return err
	}
	if status != syscall.WAIT_OBJECT_0 {
		return errors.New("runtimeprocess: unexpected wait status")
	}
	var code uint32
	if err = syscall.GetExitCodeProcess(n.process, &code); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
