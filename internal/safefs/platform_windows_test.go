//go:build windows

package safefs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPersistParentWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.wav")
	if err := os.WriteFile(path, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PersistParent(path); err != nil {
		t.Fatal(err)
	}
	if err := PersistParent(filepath.Join(dir, "missing", "result.wav")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := PersistParent("relative.wav"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestPersistParentsWindows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "source_1", "timbre", "stems-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "result.wav")
	if err := os.WriteFile(path, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PersistParents(path, root); err != nil {
		t.Fatal(err)
	}
	for _, outside := range []string{root, filepath.Join(filepath.Dir(root), "outside.wav"), filepath.Join(root+"-other", "result.wav")} {
		if err := PersistParents(outside, root); err == nil {
			t.Fatal("accepted outside output", outside)
		}
	}
}

func TestJunctionAndHardlink(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "a"), []byte("abc"), 0600)
	if e := os.Link(filepath.Join(d, "a"), filepath.Join(d, "hard")); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), nil, 0600)
	link := filepath.Join(d, "junction")
	if b, e := exec.Command("cmd.exe", "/c", "mklink", "/J", link, outside).CombinedOutput(); e != nil {
		t.Skipf("junction unavailable: %s %v", b, e)
	}
	defer os.Remove(link)
	if err := PersistParent(filepath.Join(link, "secret")); err == nil {
		t.Fatal("directory publication followed junction")
	}
	s, e := Open(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	entries := map[string]Entry{}
	e = s.Walk(context.Background(), Limits{}, func(a Entry) error { entries[a.Relative] = a; return nil })
	if e == nil || entries["junction"].Error == "" {
		t.Fatal("junction not diagnosed", entries, e)
	}
	if entries["a"].Identity != entries["hard"].Identity {
		t.Fatal("hardlink identity")
	}
	if _, _, e = s.OpenFile(filepath.Join("junction", "secret")); e == nil {
		t.Fatal("junction traversal")
	}
}
func TestReplaceSharingConflict(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "registry.json")
	temp := filepath.Join(d, "temp")
	os.WriteFile(target, []byte("old"), 0600)
	os.WriteFile(temp, []byte("new"), 0600)
	h, e := syscall.CreateFile(winptr(target), syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	if e = Replace(temp, target); e == nil {
		syscall.CloseHandle(h)
		t.Fatal("sharing violation expected")
	}
	syscall.CloseHandle(h)
	b, _ := os.ReadFile(target)
	if string(b) != "old" {
		t.Fatal("old file damaged")
	}
	if e = Replace(temp, target); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(target + ".bak")
	if string(b) != "old" {
		t.Fatal("backup lost")
	}
}
func TestRootRenameAnchoring(t *testing.T) {
	parent := t.TempDir()
	d := filepath.Join(parent, "root")
	os.Mkdir(d, 0700)
	os.WriteFile(filepath.Join(d, "a"), []byte("old"), 0600)
	s, e := Open(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = os.Rename(d, d+"-moved"); e != nil {
		// Windows os.Root currently excludes delete sharing. A rejected rename
		// also preserves the authorization; verify the original handle below.
		if !os.IsPermission(e) && !errors.Is(e, syscall.Errno(32)) {
			t.Fatal(e)
		}
		t.Logf("os.Root prevents root replacement through sharing protection: %v", e)
	} else {
		os.Mkdir(d, 0700)
		os.WriteFile(filepath.Join(d, "a"), []byte("new"), 0600)
	}
	f, _, e := s.OpenFile("a")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	b := make([]byte, 3)
	f.Read(b)
	if string(b) != "old" {
		t.Fatal("root redirected")
	}
}
