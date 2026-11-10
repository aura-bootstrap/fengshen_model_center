//go:build windows

package workspace

import (
	"os"
	"syscall"
)

const fileAttributeReparsePoint = 0x400

// Windows reports symlinks, directory junctions, and mount points as reparse
// points. Rejecting all reparse points is conservative and needs no x/sys use.
func isReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&fileAttributeReparsePoint != 0
}
