//go:build linux

package safefs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func localPath(string) error { return nil }
func finalPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
func Identity(f *os.File) (string, error) {
	var s syscall.Stat_t
	if e := syscall.Fstat(int(f.Fd()), &s); e != nil {
		return "", e
	}
	return fmt.Sprintf("linux:%x:%x", s.Dev, s.Ino), nil
}
func reparse(_ *os.Root, _ string, st os.FileInfo) (string, error) {
	if st.Mode()&os.ModeSymlink != 0 {
		return "symlink", nil
	}
	return "none", nil
}
func childIdentity(_ *os.Root, _ string, st os.FileInfo) (string, error) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || st.Mode()&os.ModeSymlink != 0 {
		return "", ErrBoundary
	}
	return fmt.Sprintf("linux:%x:%x", s.Dev, s.Ino), nil
}

func SecureDir(p string) error {
	if e := ValidatePath(p); e != nil {
		return e
	}
	if e := os.MkdirAll(p, 0700); e != nil {
		return e
	}
	s, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
		return ErrBoundary
	}
	if s.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return ErrBoundary
	}
	return os.Chmod(p, 0700)
}

type linuxLock struct {
	f    *os.File
	once sync.Once
	err  error
}

func (l *linuxLock) Close() error { l.once.Do(func() { l.err = l.f.Close() }); return l.err }
func Lock(p string) (io.Closer, error) {
	if e := ValidatePath(p); e != nil {
		return nil, e
	}
	fd, e := syscall.Open(p, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), p)
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() {
		f.Close()
		return nil, ErrBoundary
	}
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("data_locked: %w", e)
	}
	return &linuxLock{f: f}, nil
}

// PersistParent flushes directory metadata after publishing an already synced file.
func PersistParent(path string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	dir := filepath.Dir(filepath.Clean(path))
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	actual, err := finalPath(f)
	if err != nil {
		return err
	}
	if !samePath(actual, dir) {
		return ErrBoundary
	}
	return f.Sync()
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
	if _, e := os.Stat(target); e == nil {
		b := target + ".bak"
		if e = checkOrdinary(b); e != nil {
			return e
		}
		f, e := os.OpenFile(b, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if e != nil {
			return e
		}
		src, e := os.Open(target)
		if e != nil {
			f.Close()
			return e
		}
		_, e = io.Copy(f, src)
		src.Close()
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if e := os.Rename(temp, target); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(target))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
