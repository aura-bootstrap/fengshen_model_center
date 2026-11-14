package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
)

// resolveStateDir anchors center state (manifest/runtime data) to the client
// install dir: explicit flag, then FENGSHEN_STATE_DIR, then the exe's
// grandparent (bin\ layout).
func resolveStateDir(override string) string {
	if override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return abs
		}
	}
	if env := strings.TrimSpace(os.Getenv("FENGSHEN_STATE_DIR")); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
	}
	if exe, err := os.Executable(); err == nil {
		state := filepath.Dir(filepath.Dir(exe))
		// Installed layout: <client>\bin\model-center\bin\exe; the engine exe
		// two levels up marks the client install root (same test as run.bat).
		if _, err := os.Stat(filepath.Join(state, "..", "..", "fengshen-engine.exe")); err == nil {
			if abs, err := filepath.Abs(filepath.Join(state, "..", "..")); err == nil {
				return abs
			}
		}
		return state
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// stateDirExplicit reports whether the operator pinned the state dir (sandbox
// or per-instance isolation). Normal runs anchor the manifest to the slicer
// channel config roots; explicit runs stay self-contained.
func stateDirExplicit(override string) bool {
	return override != "" || strings.TrimSpace(os.Getenv("FENGSHEN_STATE_DIR")) != ""
}

// manifestTargets lists manifest landing paths in read priority order. The
// center serves all three slicer channels, so normal mode keeps one synced copy
// per channel config root (~/.fengshen/slicer/<ch>).
func manifestTargets(stateDir string, explicit bool) []string {
	if explicit {
		return []string{modelmanifest.DefaultPath(stateDir)}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{modelmanifest.DefaultPath(stateDir)}
	}
	var targets []string
	for _, ch := range []string{"user", "dev", "admin"} {
		targets = append(targets, filepath.Join(home, ".fengshen", "slicer", ch, "model_manifest.json"))
	}
	return targets
}

// loadManifestSynced loads the first existing manifest among targets (legacy
// state-dir copy as fallback) and mirrors it to every target. The deprecated
// state-dir copy is removed once all targets hold the same content.
func loadManifestSynced(targets []string, legacy string) (string, modelmanifest.Manifest, []byte, error) {
	candidates := targets
	if legacy != targets[0] {
		candidates = append(append([]string{}, targets...), legacy)
	}
	for _, path := range candidates {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		m, raw, err := modelmanifest.Load(path)
		if err != nil {
			return path, m, raw, err
		}
		synced := true
		for _, target := range targets {
			if target == path {
				continue
			}
			if cur, err := os.ReadFile(target); err == nil && string(cur) == string(raw) {
				continue
			}
			if err := modelmanifest.WriteBytes(target, raw); err != nil {
				synced = false
			}
		}
		if synced && legacy != targets[0] {
			_ = os.Remove(legacy)
		}
		return path, m, raw, nil
	}
	m, raw, err := modelmanifest.Load(targets[0])
	return targets[0], m, raw, err
}

// migrateLegacy moves pre-existing user-profile state into the install-dir
// scheme (best-effort) and removes the legacy dirs once empty.
func migrateLegacy(stateDir, manifestPath, dataDir string) {
	if home, err := os.UserHomeDir(); err == nil {
		legacyDir := filepath.Join(home, ".fengshen")
		moveIfAbsent(filepath.Join(legacyDir, "model_manifest.json"), manifestPath)
		_ = os.Remove(legacyDir)
	}
	if base, err := os.UserConfigDir(); err == nil {
		legacyParent := filepath.Join(base, "FengshenSlicer")
		moveDirIfAbsent(filepath.Join(legacyParent, "model-center"), dataDir)
		moveDirIfAbsent(filepath.Join(legacyParent, "model-center-integration"), filepath.Join(stateDir, "model-center-integration"))
		_ = os.Remove(legacyParent)
	}
}

// moveDirIfAbsent relocates a legacy directory; os.Rename fails across
// volumes (e.g. C:\Users -> W:\ install), so fall back to a recursive copy.
func moveDirIfAbsent(src, dst string) {
	if _, err := os.Stat(src); err != nil {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.RemoveAll(src)
		return
	}
	if err := os.Rename(src, dst); err == nil {
		return
	}
	if err := copyDir(src, dst); err != nil {
		return
	}
	_ = os.RemoveAll(src)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}

func moveIfAbsent(src, dst string) {
	if _, err := os.Stat(src); err != nil {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(src)
		return
	}
	if err := os.Rename(src, dst); err == nil {
		return
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return
	}
	_ = os.Remove(src)
}