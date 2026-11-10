//go:build windows

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var advapi = syscall.NewLazyDLL("advapi32.dll")
var createNamedPipe = kernel.NewProc("CreateNamedPipeW")
var connectNamedPipe = kernel.NewProc("ConnectNamedPipe")
var getPipePID = kernel.NewProc("GetNamedPipeServerProcessId")
var queryImage = kernel.NewProc("QueryFullProcessImageNameW")
var cancelIO = kernel.NewProc("CancelIoEx")
var createEvent = kernel.NewProc("CreateEventW")
var overlappedResult = kernel.NewProc("GetOverlappedResult")
var disconnectPipe = kernel.NewProc("DisconnectNamedPipe")
var flushPipe = kernel.NewProc("FlushFileBuffers")
var convertSD = advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
var localFree = kernel.NewProc("LocalFree")

func platformSupported() error { return nil }

// Reject every reparse component, not just the final executable. Integration
// uses fixed local delivery paths and has no reason to follow junctions.
func platformCheckPath(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		attr, ok := st.Sys().(*syscall.Win32FileAttributeData)
		if !ok || attr.FileAttributes&0x400 != 0 {
			return errors.New("center_reparse_denied")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}
func currentSID() (string, error) {
	var token syscall.Token
	process, e := syscall.GetCurrentProcess()
	if e != nil {
		return "", e
	}
	if e := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); e != nil {
		return "", e
	}
	defer token.Close()
	u, e := token.GetTokenUser()
	if e != nil {
		return "", e
	}
	return u.User.Sid.String()
}
func pipeName(data string) (string, error) {
	sid, e := currentSID()
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256([]byte(sid + "\x00" + strings.ToLower(filepath.Clean(data))))
	return `\\.\pipe\fengshen-model-center-` + hex.EncodeToString(sum[:16]), nil
}
func queryProcess(pid uint32) (processIdentity, error) {
	var out processIdentity
	h, e := syscall.OpenProcess(0x1000, false, pid)
	if e != nil {
		return out, e
	}
	defer syscall.CloseHandle(h)
	var created, exit, kernelTime, user syscall.Filetime
	if e = syscall.GetProcessTimes(h, &created, &exit, &kernelTime, &user); e != nil {
		return out, e
	}
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		return out, errors.New("center_process_exited")
	}
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	r, _, ce := queryImage.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return out, ce
	}
	image := syscall.UTF16ToString(buf[:n])
	id, size, mod, e := metadata(image)
	if e != nil {
		return out, e
	}
	var token syscall.Token
	if e = syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &token); e != nil {
		return out, e
	}
	u, e := token.GetTokenUser()
	token.Close()
	if e != nil {
		return out, e
	}
	sid, e := u.User.Sid.String()
	if e != nil {
		return out, e
	}
	out = processIdentity{PID: pid, Created: fmt.Sprintf("%08x%08x", created.HighDateTime, created.LowDateTime), Image: image, FileID: id, Size: size, Modified: mod, SID: sid}
	return out, nil
}

// pipePeer obtains the kernel-reported client PID on the server handle. The
// SID is then queried from that actual process token, in addition to pipe DACL.
func pipePeer(rw io.ReadWriter) (processIdentity, error) {
	p, ok := rw.(*winPipe)
	if !ok {
		return processIdentity{}, errors.New("center_pipe_peer_invalid")
	}
	p.mu.Lock()
	if p.closed || p.interrupted {
		p.mu.Unlock()
		return processIdentity{}, os.ErrClosed
	}
	var pid uint32
	r, _, _ := kernel.NewProc("GetNamedPipeClientProcessId").Call(uintptr(p.h), uintptr(unsafe.Pointer(&pid)))
	p.mu.Unlock()
	if r == 0 || pid == 0 {
		return processIdentity{}, errors.New("center_pipe_peer_invalid")
	}
	return queryProcess(pid)
}
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
}
func startupLock(ctx context.Context, path string) (io.Closer, error) {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		l, e := safefs.Lock(path)
		if e == nil {
			return l, nil
		}
		if !errors.Is(e, syscall.Errno(32)) && !errors.Is(e, syscall.Errno(33)) {
			return nil, e
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

type winPipe struct {
	mu          sync.Mutex
	h           syscall.Handle
	closed      bool
	interrupted bool
	server      bool
	active      sync.WaitGroup
	closeDone   chan struct{}
	closeErr    error
}

func (p *winPipe) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		return p.closeErr
	}
	p.closed = true
	p.closeDone = make(chan struct{})
	cancelIO.Call(uintptr(p.h), 0)
	if p.server {
		// FlushFileBuffers is synchronous even on an overlapped handle.
		// Disconnect releases its wait and discards an unconsumed response.
		disconnectPipe.Call(uintptr(p.h))
	}
	p.mu.Unlock()
	// No new operation can start. Keep the handle valid until every submitted
	// operation (including canceled ones) has actually completed.
	p.active.Wait()
	p.closeErr = syscall.CloseHandle(p.h)
	close(p.closeDone)
	return p.closeErr
}

// io submits under mu, so cancellation cannot miss an operation between its
// state check and kernel submission. Each pending operation owns its event.
func (p *winPipe) io(b []byte, operation string) (int, error) {
	p.mu.Lock()
	if p.closed || p.interrupted {
		p.mu.Unlock()
		return 0, os.ErrClosed
	}
	p.active.Add(1)
	defer p.active.Done()
	// Windows retains these addresses beyond submission until completion.
	var pinned runtime.Pinner
	defer pinned.Unpin()
	if len(b) != 0 {
		pinned.Pin(&b[0])
	}
	event, _, e := createEvent.Call(0, 1, 0, 0)
	if event == 0 {
		p.mu.Unlock()
		return 0, e
	}
	defer syscall.CloseHandle(syscall.Handle(event))
	o := syscall.Overlapped{HEvent: syscall.Handle(event)}
	pinned.Pin(&o)
	h := p.h
	var n uint32
	pinned.Pin(&n)
	var err error
	switch operation {
	case "read":
		err = syscall.ReadFile(h, b, &n, &o)
	case "write":
		err = syscall.WriteFile(h, b, &n, &o)
	case "connect":
		r, _, ce := connectNamedPipe.Call(uintptr(h), uintptr(unsafe.Pointer(&o)))
		if r == 0 && ce != syscall.Errno(535) { // ERROR_PIPE_CONNECTED
			err = ce
		}
	}
	if err != syscall.ERROR_IO_PENDING {
		p.mu.Unlock()
		return int(n), err
	}
	p.mu.Unlock()
	r, _, ce := overlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&o)), uintptr(unsafe.Pointer(&n)), 1)
	if r == 0 {
		return int(n), ce
	}
	return int(n), nil
}
func (p *winPipe) Read(b []byte) (int, error)  { return p.io(b, "read") }
func (p *winPipe) Write(b []byte) (int, error) { return p.io(b, "write") }

func (p *winPipe) flush() {
	p.mu.Lock()
	if p.closed || p.interrupted {
		p.mu.Unlock()
		return
	}
	h := p.h
	p.active.Add(1)
	p.mu.Unlock()
	defer p.active.Done()
	// If interrupted before this syscall, the disconnected instance makes the
	// call fail immediately. Reconnection waits for this call to return.
	flushPipe.Call(uintptr(h))
}

func (p *winPipe) interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.interrupted {
		return
	}
	p.interrupted = true
	cancelIO.Call(uintptr(p.h), 0)
	disconnectPipe.Call(uintptr(p.h))
}

func (p *winPipe) reconnect() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	r, _, ce := disconnectPipe.Call(uintptr(p.h))
	if r == 0 && ce != syscall.Errno(233) && ce != syscall.Errno(232) {
		return ce
	}
	p.interrupted = false
	return nil
}
func listenPipe(name string) (io.Closer, error) {
	sid, e := currentSID()
	if e != nil {
		return nil, e
	}
	// Protected DACL grants only the current user; reject remote clients and
	// require the first instance so a squatted pipe is never silently reused.
	text, e := syscall.UTF16PtrFromString("D:P(A;;GA;;;" + sid + ")")
	if e != nil {
		return nil, e
	}
	var sd uintptr
	r, _, ce := convertSD.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return nil, ce
	}
	defer localFree.Call(sd)
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	p, _ := syscall.UTF16PtrFromString(name)
	h, _, ce := createNamedPipe.Call(uintptr(unsafe.Pointer(p)), 0x00000003|0x00080000|syscall.FILE_FLAG_OVERLAPPED, 0x00000008, 1, 16384, 16384, 2000, uintptr(unsafe.Pointer(&sa)))
	if h == uintptr(syscall.InvalidHandle) {
		return nil, ce
	}
	return &winPipe{h: syscall.Handle(h), server: true}, nil
}
func servePipe(ctx context.Context, closer io.Closer, handler func(io.ReadWriter)) error {
	p := closer.(*winPipe)
	stop, stopped := make(chan struct{}), make(chan struct{})
	defer func() {
		close(stop)
		<-stopped
		if ctx.Err() != nil {
			p.Close()
		}
	}()
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			p.Close()
		case <-stop:
		}
	}()
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		_, e := p.io(nil, "connect")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e != nil {
			if e == syscall.Errno(232) || e == syscall.Errno(233) {
				if e = p.reconnect(); e == nil {
					continue
				}
			}
			return e
		}
		timedOut := make(chan struct{})
		timer := time.AfterFunc(2*time.Second, func() {
			defer close(timedOut)
			p.interrupt()
		})
		handler(p)
		p.flush()
		if !timer.Stop() {
			// Stop alone does not join an already running callback. Never let
			// the previous watchdog disconnect a subsequent client.
			<-timedOut
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = p.reconnect(); e != nil {
			return e
		}
	}
}
func dialPipe(ctx context.Context, name string) (io.ReadWriteCloser, uint32, error) {
	var p *winPipe
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return nil, 0, e
		}
		ptr, _ := syscall.UTF16PtrFromString(name)
		h, e := syscall.CreateFile(ptr, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
		if e == nil {
			p = &winPipe{h: h}
			break
		}
		if e != syscall.Errno(231) && e != syscall.Errno(233) {
			return nil, 0, e
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-tick.C:
		}
	}
	var pid uint32
	r, _, e := getPipePID.Call(uintptr(p.h), uintptr(unsafe.Pointer(&pid)))
	if r == 0 {
		p.Close()
		return nil, 0, e
	}
	timer := time.AfterFunc(2*time.Second, func() { p.Close() })
	return &timedPipe{winPipe: p, timer: timer}, pid, nil
}

type timedPipe struct {
	*winPipe
	timer *time.Timer
}

func (p *timedPipe) Close() error { p.timer.Stop(); return p.winPipe.Close() }
