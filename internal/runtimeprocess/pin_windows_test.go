//go:build windows

package runtimeprocess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedModelCannotBeReplacedOrWritten(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model.bin")
	if err := os.WriteFile(path, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	directory, err := PinDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	file, err := PinFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if writer, err := os.OpenFile(path, os.O_WRONLY, 0600); err == nil {
		writer.Close()
		file.Close()
		t.Fatal("pinned file was writable")
	}
	if err := os.Rename(path, path+".moved"); err == nil {
		file.Close()
		t.Fatal("pinned file was replaceable")
	}
	if err := os.Rename(root, root+".moved"); err == nil {
		file.Close()
		t.Fatal("pinned directory was movable")
	}
	file.Close()
	if writer, err := os.OpenFile(path, os.O_WRONLY, 0600); err != nil {
		t.Fatal(err)
	} else {
		writer.Close()
	}
}
