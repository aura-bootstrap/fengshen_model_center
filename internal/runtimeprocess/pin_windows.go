//go:build windows

package runtimeprocess

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func PinFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrIdentity
	}
	f, _, canonical, err := openImage(path)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(filepath.Clean(path), filepath.Clean(canonical)) {
		f.Close()
		return nil, ErrIdentity
	}
	return f, nil
}

func PinDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrIdentity
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, ErrIdentity
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0x02200000, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	var info syscall.ByHandleFileInformation
	if err = syscall.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 {
		f.Close()
		return nil, ErrIdentity
	}
	return f, nil
}
