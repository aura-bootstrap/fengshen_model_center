//go:build windows

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var queryImage = kernel.NewProc("QueryFullProcessImageNameW")

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
	out = processIdentity{PID: pid, Created: fmt.Sprintf("%08x%08x", created.HighDateTime, created.LowDateTime), Image: image, FileID: id, Size: size, Modified: mod}
	return out, nil
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
