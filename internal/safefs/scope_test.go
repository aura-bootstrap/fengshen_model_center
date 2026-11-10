package safefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScopeBoundary(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "a.bin")
	os.WriteFile(p, []byte("abc"), 0600)
	os.WriteFile(filepath.Join(d, "b.bin"), nil, 0600)
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, _, e = s.OpenFile("b.bin"); e == nil {
		t.Fatal("sibling authorized")
	}
	f, a, e := s.OpenFile("a.bin")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if a.Identity == "" || a.Size != 3 {
		t.Fatal(a)
	}
	if _, _, e = s.OpenFile("../a.bin"); e == nil {
		t.Fatal("escape")
	}
	os.Rename(p, p+".old")
	os.WriteFile(p, nil, 0600)
	if _, _, e = s.OpenFile("a.bin"); e == nil {
		t.Fatal("replacement authorized")
	}
}
func TestWalkLimits(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "a"), nil, 0600)
	os.WriteFile(filepath.Join(d, "b"), nil, 0600)
	s, e := Open(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	n := 0
	e = s.Walk(context.Background(), Limits{MaxEntries: 1, MaxDepth: 2}, func(Entry) error { n++; return nil })
	if !errors.Is(e, ErrLimit) || n != 1 {
		t.Fatalf("%d %v", n, e)
	}
	ctx, c := context.WithCancel(context.Background())
	c()
	if !errors.Is(s.Walk(ctx, Limits{}, func(Entry) error { return nil }), context.Canceled) {
		t.Fatal("cancel")
	}
}
func TestWalkDepthBoundary(t *testing.T) {
	d := t.TempDir()
	current := d
	for n := 0; n < 64; n++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0700); err != nil {
			t.Fatal(err)
		}
		if n == 62 || n == 63 {
			if err := os.WriteFile(filepath.Join(current, "weight.bin"), []byte("abc"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(d, "z-sibling.bin"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, depth := range []int{64, 65} {
		seen := map[string]Entry{}
		err := s.Walk(context.Background(), Limits{MaxDepth: depth}, func(a Entry) error {
			seen[a.Relative] = a
			return nil
		})
		if depth == 64 && (!errors.Is(err, ErrPartial) || !errors.Is(err, ErrLimit)) || depth == 65 && err != nil {
			t.Fatalf("depth=%d: %v", depth, err)
		}
		parent := filepath.Dir(current)
		inside, _ := filepath.Rel(d, filepath.Join(parent, "weight.bin"))
		beyond, _ := filepath.Rel(d, filepath.Join(current, "weight.bin"))
		if _, ok := seen[inside]; !ok {
			t.Fatalf("depth=%d omitted boundary file", depth)
		}
		if _, ok := seen[beyond]; ok != (depth == 65) {
			t.Fatalf("depth=%d traversed wrong level: present=%v", depth, ok)
		}
		if _, ok := seen["z-sibling.bin"]; !ok {
			t.Fatalf("depth=%d stopped before later sibling", depth)
		}
	}
}

func TestRejectPaths(t *testing.T) {
	for _, p := range []string{"relative", `\\server\share\a`, `\\?\C:\a`, `C:\a:stream`, `C:\`, `C:\foo\NUL`} {
		if ValidatePath(p) == nil {
			t.Errorf("accepted %q", p)
		}
	}
}
func TestLinksAndDiagnostics(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "a"), []byte("ok"), 0600)
	if e := os.Symlink("a", filepath.Join(d, "inside")); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(d, "outside"))
	os.Symlink(".", filepath.Join(d, "dirlink"))
	s, e := Open(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	seen := map[string]Entry{}
	e = s.Walk(context.Background(), Limits{}, func(a Entry) error { seen[a.Relative] = a; return nil })
	if e == nil {
		t.Fatal("missing partial diagnostic")
	}
	if seen["a"].Identity == "" || seen["inside"].Identity != seen["a"].Identity || seen["outside"].Error == "" {
		t.Fatal(seen)
	}
	if seen["inside"].SourcePath != filepath.Join(d, "inside") || seen["inside"].Path != seen["a"].Path {
		t.Fatal("request source and handle target were conflated", seen)
	}
}
