package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
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

func TestManifestTargetsNormalAndExplicit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("FENGSHEN_STATE_DIR", "")
	stateDir := t.TempDir()
	normal := manifestTargets(stateDir, false)
	if len(normal) != 3 {
		t.Fatalf("normal targets=%v", normal)
	}
	for i, ch := range []string{"user", "dev", "admin"} {
		if want := filepath.Join(home, ".fengshen", "slicer", ch, "model_manifest.json"); normal[i] != want {
			t.Fatalf("target[%d]=%q want %q", i, normal[i], want)
		}
	}
	explicit := manifestTargets(stateDir, true)
	if len(explicit) != 1 || explicit[0] != modelmanifest.DefaultPath(stateDir) {
		t.Fatalf("explicit targets=%v", explicit)
	}
	t.Setenv("FENGSHEN_STATE_DIR", stateDir)
	if !stateDirExplicit("") {
		t.Fatal("FENGSHEN_STATE_DIR should pin the state dir")
	}
}

func writeManifestFixture(t *testing.T, path string, bytes int64) []byte {
	t.Helper()
	m := modelmanifest.Manifest{Schema: 1, Entries: []modelmanifest.Entry{{ModelID: "whisper-asr", Path: filepath.Join(filepath.Dir(path), "a.bin"), Bytes: bytes}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLoadManifestSyncedMigratesLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("FENGSHEN_STATE_DIR", "")
	stateDir := t.TempDir()
	targets := manifestTargets(stateDir, false)
	legacy := modelmanifest.DefaultPath(stateDir)
	want := writeManifestFixture(t, legacy, 3)

	path, m, raw, err := loadManifestSynced(targets, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if path != legacy || m.Schema != 1 || string(raw) != string(want) {
		t.Fatalf("loaded path=%q schema=%d", path, m.Schema)
	}
	for _, target := range targets {
		got, err := os.ReadFile(target)
		if err != nil || string(got) != string(want) {
			t.Fatalf("mirror %s: %v", target, err)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy manifest should be removed after sync: %v", err)
	}
}

func TestLoadManifestSyncedFirstExistingWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("FENGSHEN_STATE_DIR", "")
	stateDir := t.TempDir()
	targets := manifestTargets(stateDir, false)
	want := writeManifestFixture(t, targets[0], 3)
	writeManifestFixture(t, targets[1], 9)

	path, _, raw, err := loadManifestSynced(targets, modelmanifest.DefaultPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if path != targets[0] || string(raw) != string(want) {
		t.Fatalf("loaded path=%q", path)
	}
	got, err := os.ReadFile(targets[1])
	if err != nil || string(got) != string(want) {
		t.Fatalf("dev mirror should be overwritten: %v", err)
	}
}

func TestLoadManifestSyncedExplicitStaysInStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	stateDir := t.TempDir()
	targets := manifestTargets(stateDir, true)
	legacy := modelmanifest.DefaultPath(stateDir)
	writeManifestFixture(t, legacy, 3)

	path, _, _, err := loadManifestSynced(targets, legacy)
	if err != nil || path != legacy {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".fengshen")); !os.IsNotExist(err) {
		t.Fatalf("explicit mode must not create user dirs: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("explicit mode must keep state-dir manifest: %v", err)
	}
}