//go:build !windows

package runtimeassets

import "os"

func ordinaryDirectory(st os.FileInfo) bool { return st.IsDir() && st.Mode()&os.ModeSymlink == 0 }
