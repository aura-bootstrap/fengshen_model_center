//go:build windows

package safefs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var advapi = syscall.NewLazyDLL("advapi32.dll")

func winptr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func localPath(p string) error {
	v := filepath.VolumeName(p)
	if len(v) != 2 || v[1] != ':' {
		return ErrBoundary
	}
	r, _, e := kernel.NewProc("GetDriveTypeW").Call(uintptr(unsafe.Pointer(winptr(v + `\`))))
	if r != 3 && r != 6 {
		return fmt.Errorf("path_invalid: 非本机固定盘 (%d): %v", r, e)
	}
	return nil
}
func finalPath(f *os.File) (string, error) {
	b := make([]uint16, 32768)
	n, _, e := kernel.NewProc("GetFinalPathNameByHandleW").Call(f.Fd(), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0)
	if n == 0 || n >= uintptr(len(b)) {
		return "", e
	}
	p := syscall.UTF16ToString(b[:n])
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return "", ErrBoundary
	}
	p = strings.TrimPrefix(p, `\\?\`)
	return p, nil
}
func Identity(f *os.File) (string, error) {
	var i struct {
		Volume uint64
		FileID [16]byte
	}
	r, _, e := kernel.NewProc("GetFileInformationByHandleEx").Call(f.Fd(), 18, uintptr(unsafe.Pointer(&i)), unsafe.Sizeof(i))
	if r == 0 {
		return "", fmt.Errorf("identity_unavailable: %w", e)
	}
	return fmt.Sprintf("windows:%016x:%x", i.Volume, i.FileID), nil
}
func reparse(root *os.Root, rel string, st os.FileInfo) (string, error) {
	a, ok := st.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return "", ErrBoundary
	}
	if a.FileAttributes&0x400 == 0 {
		return "none", nil
	}
	anchor, e := root.Open(".")
	if e != nil {
		return "", e
	}
	base, e := finalPath(anchor)
	anchor.Close()
	if e != nil {
		return "", e
	}
	p := filepath.Join(base, rel)
	h, e := syscall.CreateFile(winptr(p), 0, 7, nil, syscall.OPEN_EXISTING, 0x02200000, 0)
	if e != nil {
		return "", e
	}
	defer syscall.CloseHandle(h)
	var info struct{ Attributes, Tag uint32 }
	r, _, e := kernel.NewProc("GetFileInformationByHandleEx").Call(uintptr(h), 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		return "", e
	}
	if info.Tag == 0xa000000c {
		return "symlink", nil
	}
	if info.Tag == 0xa0000003 {
		return "junction", nil
	}
	return "reparse", nil
}

// childIdentity samples a no-follow handle while the parent Root remains pinned.
// Derive calls this only for one path component, never an unchecked ancestor chain.
func childIdentity(root *os.Root, rel string, _ os.FileInfo) (string, error) {
	anchor, err := root.Open(".")
	if err != nil {
		return "", err
	}
	base, err := finalPath(anchor)
	anchor.Close()
	if err != nil {
		return "", err
	}
	expected := filepath.Join(base, rel)
	h, err := syscall.CreateFile(winptr(expected), 0, 7, nil, syscall.OPEN_EXISTING, 0x02200000, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(h), expected)
	defer f.Close()
	var info struct{ Attributes, Tag uint32 }
	r, _, err := kernel.NewProc("GetFileInformationByHandleEx").Call(f.Fd(), 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		return "", err
	}
	actual, err := finalPath(f)
	if err != nil || info.Attributes&0x400 != 0 || !samePath(actual, expected) {
		return "", ErrBoundary
	}
	return Identity(f)
}

func SecureDir(p string) error {
	if e := ValidatePath(p); e != nil {
		return e
	}
	if e := os.MkdirAll(p, 0700); e != nil {
		return e
	}
	st, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return ErrBoundary
	}
	if a, ok := st.Sys().(*syscall.Win32FileAttributeData); !ok || a.FileAttributes&0x400 != 0 {
		return ErrBoundary
	}
	token, e := syscall.OpenCurrentProcessToken()
	if e != nil {
		return e
	}
	defer token.Close()
	u, e := token.GetTokenUser()
	if e != nil {
		return e
	}
	sid, e := u.User.Sid.String()
	if e != nil {
		return e
	}
	sddl := "D:P(A;OICI;FA;;;" + sid + ")"
	var sd uintptr
	r, _, e := advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(winptr(sddl))), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return e
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	r, _, e = advapi.NewProc("SetFileSecurityW").Call(uintptr(unsafe.Pointer(winptr(p))), 0x80000004, sd)
	if r == 0 {
		return e
	}
	return nil
}

type winLock struct {
	f    *os.File
	once sync.Once
	err  error
}

func (l *winLock) Close() error { l.once.Do(func() { l.err = l.f.Close() }); return l.err }
func Lock(p string) (io.Closer, error) {
	if e := ValidatePath(p); e != nil {
		return nil, e
	}
	if e := checkOrdinary(p); e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(winptr(p), syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, 0x00200000, 0)
	if e != nil {
		return nil, fmt.Errorf("data_locked: %w", e)
	}
	f := os.NewFile(uintptr(h), p)
	var info syscall.ByHandleFileInformation
	if e = syscall.GetFileInformationByHandle(h, &info); e != nil || info.FileAttributes&0x400 != 0 {
		f.Close()
		return nil, ErrBoundary
	}
	return &winLock{f: f}, nil
}

// PersistParent flushes directory metadata after publishing an already synced file.
func PersistParent(path string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	dir := filepath.Dir(filepath.Clean(path))
	h, err := syscall.CreateFile(winptr(dir), syscall.GENERIC_WRITE, 7, nil, syscall.OPEN_EXISTING, 0x02200000, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), dir)
	defer f.Close()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	actual, err := finalPath(f)
	if err != nil {
		return err
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&0x400 != 0 || !samePath(actual, dir) {
		return ErrBoundary
	}
	return syscall.FlushFileBuffers(h)
}

func Replace(temp, target string) error {
	if filepath.Dir(temp) != filepath.Dir(target) {
		return ErrBoundary
	}
	if e := checkOrdinary(target); e != nil {
		return e
	}
	if e := checkOrdinary(temp); e != nil {
		return e
	}
	_, e := os.Lstat(target)
	var r uintptr
	if os.IsNotExist(e) {
		r, _, e = kernel.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(winptr(temp))), uintptr(unsafe.Pointer(winptr(target))), 8)
	} else if e == nil {
		if e = checkOrdinary(target + ".bak"); e != nil {
			return e
		}
		r, _, e = kernel.NewProc("ReplaceFileW").Call(uintptr(unsafe.Pointer(winptr(target))), uintptr(unsafe.Pointer(winptr(temp))), uintptr(unsafe.Pointer(winptr(target+".bak"))), 0, 0, 0)
	} else {
		return e
	}
	if r == 0 {
		return e
	}
	return nil
}
