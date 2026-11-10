package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "models", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "models", "only.bin"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, err := s.Derive(filepath.Join("models", "only.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if f, _, err := file.OpenFile("nested/other.bin"); err == nil {
		f.Close()
		t.Fatal("derived file authorized siblings")
	}
	if _, err := file.Derive("."); !errors.Is(err, ErrBoundary) {
		t.Fatal("file scope widened", err)
	}
	if _, err := s.Derive("missing/child"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing root classification", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, rel := range []string{"linked", "linked/missing"} {
		if derived, err := s.Derive(rel); !errors.Is(err, ErrBoundary) {
			if derived != nil {
				derived.Close()
			}
			t.Fatalf("directory link not rejected: %s: %v", rel, err)
		}
	}
	if err := os.Symlink("models/only.bin", filepath.Join(root, "leaf.bin")); err != nil {
		t.Fatal(err)
	}
	if derived, err := s.Derive("leaf.bin"); err == nil {
		derived.Close()
		t.Fatal("default leaf link accepted")
	}
}

func TestOpenDirectoryScoped(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "models"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, a, err := s.OpenDirectory("models")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !a.IsDir || a.LinkKind != "none" || a.Identity == "" {
		t.Fatalf("%+v", a)
	}
	for _, rel := range []string{"file", "../outside"} {
		if f, _, err := s.OpenDirectory(rel); err == nil {
			f.Close()
			t.Fatalf("accepted %s", rel)
		}
	}
	if err := os.Symlink(filepath.Join(root, "models"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if f, _, err := s.OpenDirectory("linked"); err == nil {
		f.Close()
		t.Fatal("directory link accepted")
	}
}
