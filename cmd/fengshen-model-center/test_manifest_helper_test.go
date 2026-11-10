package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
)

// testManifest builds an in-memory manifest from fixture roots using the same
// recognition rules as the production generator. Tests no longer rely on
// directory scanning or a registration file.
func testManifest(t *testing.T, roots ...string) modelmanifest.Manifest {
	t.Helper()
	m := modelmanifest.Manifest{Schema: 1}
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			for _, model := range catalog() {
				if !recognizes(model, p) && !allowed(model, p) {
					continue
				}
				key := model.ID + "\x00" + filepath.Clean(p)
				if !seen[key] {
					seen[key] = true
					info, _ := d.Info()
					m.Entries = append(m.Entries, modelmanifest.Entry{ModelID: model.ID, Role: "weight", Path: p, Bytes: info.Size()})
				}
				break
			}
			return nil
		})
	}
	return m
}

func testManifestForSite(t *testing.T, root, home string) modelmanifest.Manifest {
	t.Helper()
	return testManifest(t,
		filepath.Join(root, "sidecar", "seed-vc", "checkpoints"),
		filepath.Join(root, "models"),
		filepath.Join(home, ".cache", "torch", "hub", "checkpoints"),
	)
}

// allowFixtureRoots makes fixture managers permissive: every catalog model may
// be discovered under the given directories without per-file manifest entries.
// Production always filters by the user manifest.
func allowFixtureRoots(m *Manager, dirs ...string) {
	var ranges []scanRange
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		for _, model := range catalog() {
			ranges = append(ranges, scanRange{path: dir, kind: "manifest", models: []string{model.ID}})
		}
	}
	m.extraRanges = ranges
	m.registrySnap.Registrations = map[string]registry.Registration{}
	m.registrySnap.Revision = 0
	m.manifestRevision = 0
	m.manifestFiles = nil
}
