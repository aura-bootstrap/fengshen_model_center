//go:build !windows

package workspace

import "os"

func isReparsePoint(info os.FileInfo) bool { return false }
