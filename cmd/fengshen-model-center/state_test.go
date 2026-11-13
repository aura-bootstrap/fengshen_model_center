package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyMovesProfileState(t *testing.T) {
	home := t.TempDir()
	appdata := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", appdata)

	put := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacyDir := filepath.Join(home, ".fengshen")
	put(filepath.Join(legacyDir, "model_manifest.json"), "manifest")
	legacyParent := filepath.Join(appdata, "FengshenSlicer")
	put(filepath.Join(legacyParent, "model-center", "bindings.json"), "data")
	put(filepath.Join(legacyParent, "model-center-integration", "instance.json"), "integration")

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "model_manifest.json")
	dataDir := filepath.Join(stateDir, "model-center")
	migrateLegacy(stateDir, manifestPath, dataDir)

	for path, want := range map[string]string{
		manifestPath: "manifest",
		filepath.Join(dataDir, "bindings.json"):                              "data",
		filepath.Join(stateDir, "model-center-integration", "instance.json"): "integration",
	} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("migrated %s: got %q, %v; want %q", path, raw, err, want)
		}
	}
	for _, gone := range []string{legacyDir, legacyParent} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("legacy dir still present: %s (%v)", gone, err)
		}
	}
}

func TestCopyDirRecursive(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copied")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "a.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "nested", "a.bin"))
	if err != nil || len(raw) != 3 || raw[2] != 2 {
		t.Fatalf("copied file: %v %v", raw, err)
	}
}