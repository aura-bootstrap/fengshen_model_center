package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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