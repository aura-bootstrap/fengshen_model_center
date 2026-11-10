//go:build windows

package runtimeassets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJunctionRootRejected(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "junction")
	out, e := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if e != nil {
		t.Fatalf("create junction: %v: %s", e, out)
	}
	defer os.Remove(link)
	if s, e := New(link); e == nil {
		s.Close()
		t.Fatal("junction accepted")
	}
	if s, e := New(filepath.Join(link, "child")); e == nil {
		s.Close()
		t.Fatal("junction ancestor accepted")
	}
}
func TestReparseResultDirectoryRejected(t *testing.T) {
	root := t.TempDir()
	s, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	target := t.TempDir()
	link := filepath.Join(s.scope.Path(), "junction")
	out, e := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if e != nil {
		t.Fatalf("create junction: %v: %s", e, out)
	}
	defer os.Remove(link)
	// IDs are looked up, never traversed as filesystem names.
	if _, _, e = s.Download(context.Background(), "t", "junction/anything"); e == nil {
		t.Fatal("reparse path accepted")
	}
}
