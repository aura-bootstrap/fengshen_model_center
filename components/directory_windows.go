package components

import (
	"os"
	"syscall"
)

func ordinaryDirectory(st os.FileInfo) bool {
	a, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && st.IsDir() && st.Mode()&os.ModeSymlink == 0 && a.FileAttributes&0x400 == 0
}
