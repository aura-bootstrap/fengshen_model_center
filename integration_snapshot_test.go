package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/components"
)

func TestDependencySnapshotNoInventoryAndBoundedComponents(t *testing.T) {
	m := fixtureManager(t)
	for i := 0; i < 400; i++ {
		m.componentSnapshot = append(m.componentSnapshot, components.Evidence{ComponentID: "whisper-batch", Version: "ambiguous", Packaging: "native", Integrity: "matched"})
	}
	s := m.dependencySnapshot()
	if len(s.Components) != 7 || len(s.Models) != 12 || !s.Stale || s.Complete || s.InventoryID != "" {
		t.Fatalf("%+v", s)
	}
	for _, e := range s.Components {
		if e.StartupBuildBound || e.Integrity == "matched" {
			t.Fatal(e)
		}
	}
	for _, e := range s.Models {
		if e.NonEmpty || e.Dependency != "unknown" {
			t.Fatal(e)
		}
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), m.root) || !strings.Contains(string(b), "component_version_ambiguous") {
		t.Fatal(string(b))
	}
}

func TestDependencySnapshotModelEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, dependency, bytes, identity, evidence string
		want                                        bool
	}{
		{"leaf", "not_applicable", "3", "recognized_by_rule", "recognized_by_rule", true},
		{"dependencies", "files_found", "3", "recognized_by_rule", "recognized_by_rule", true},
		{"unknown-dependencies", "unknown", "3", "recognized_by_rule", "recognized_by_rule", true},
		{"empty", "not_applicable", "0", "recognized_by_rule", "recognized_by_rule", false},
		{"candidate", "not_applicable", "3", "unconfirmed", "candidate", false},
		{"bad-size", "not_applicable", "unknown", "recognized_by_rule", "recognized_by_rule", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManager(t)
			inv := &Inventory{ID: "inventory", FinishedAt: time.Now(), Complete: true, Models: []ModelState{{Model: Model{ID: "demucs"}, DiscoveryStatus: "found", IdentityStatus: tc.identity, CompositionStatus: "complete", DependencyStatus: tc.dependency, files: []*ModelFile{{Bytes: tc.bytes, Evidence: tc.evidence}}}}}
			m.current, m.inventories[inv.ID] = inv.ID, inv
			s := m.dependencySnapshot()
			if s.Models[0].NonEmpty != tc.want || s.Models[0].Dependency != tc.dependency || s.Stale {
				t.Fatalf("%+v", s)
			}
			inv.Models[0].files = nil
			if m.dependencySnapshot().Models[0].NonEmpty {
				t.Fatal("no file evidence admitted")
			}
		})
	}
}

func TestDependencySnapshotFreshnessAndPartial(t *testing.T) {
	for _, kind := range []string{"partial", "old", "future", "revision", "stale", "unfinished"} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			inv := &Inventory{ID: "inventory", FinishedAt: time.Now(), Complete: true}
			switch kind {
			case "partial":
				inv.Complete = false
			case "old":
				inv.FinishedAt = time.Now().Add(-6 * time.Minute)
			case "future":
				inv.FinishedAt = time.Now().Add(time.Hour)
			case "revision":
				inv.RegistryRevision = 5
			case "stale":
				inv.Stale = true
			case "unfinished":
				inv.FinishedAt = time.Time{}
			}
			m.current, m.inventories[inv.ID] = inv.ID, inv
			s := m.dependencySnapshot()
			if s.Complete != inv.Complete || s.Stale != (kind != "partial") {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func snapshotComponentFixture(t *testing.T, m *Manager) (string, components.Evidence) {
	t.Helper()
	root := t.TempDir()
	dir := components.ComponentDirectory(root, "whisper-batch", "1")
	body := []byte("not executed")
	put(t, filepath.Join(dir, "runner.exe"), string(body))
	sum := sha256.Sum256(body)
	manifest := components.Manifest{Schema: 1, ComponentID: "whisper-batch", Version: "1", OS: runtime.GOOS, Arch: runtime.GOARCH, EntryType: "executable", Entry: "runner.exe", Packaging: "native", StartupReportRef: "startup.json", Files: []components.File{{Path: "runner.exe", Bytes: "12", SHA256: hex.EncodeToString(sum[:])}}}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "manifest.json"), string(b))
	sum = sha256.Sum256(b)
	zero := 0
	report := components.StartupReport{Schema: 1, ComponentID: manifest.ComponentID, Version: manifest.Version, ManifestSHA256: hex.EncodeToString(sum[:]), StartupTest: "passed", CleanEnvironment: "not_verified", ProtocolEvidence: "not_declared", Inference: "not_tested", MetadataOnly: &components.MetadataOnlyEvidence{Arguments: []string{"--component-info"}, ExitCode: &zero, CheckedAt: time.Now(), NoModelLoad: true, NoNetwork: true, NoCompilation: true}}
	b, _ = json.Marshal(report)
	put(t, filepath.Join(dir, "startup.json"), string(b))
	m.components = components.New(root)
	e, err := m.components.Verify(context.Background(), manifest.ComponentID, manifest.Version)
	if err != nil {
		t.Fatal(err)
	}
	m.componentSnapshot = []components.Evidence{e}
	return dir, e
}

func TestDependencySnapshotStartupBuildBindingAndChanges(t *testing.T) {
	for _, kind := range []string{"bound", "no-report", "other-build", "unverified", "report", "manifest", "binary"} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			dir, e := snapshotComponentFixture(t, m)
			switch kind {
			case "no-report":
				e.StartupReport = nil
			case "other-build":
				e.StartupReport.ManifestSHA256 = strings.Repeat("0", 64)
			case "unverified":
				e.Integrity = "not_checked"
			case "report", "manifest", "binary":
				name := map[string]string{"report": "startup.json", "manifest": "manifest.json", "binary": "runner.exe"}[kind]
				if err := os.Chtimes(filepath.Join(dir, name), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			m.componentSnapshot[0] = e
			s := m.dependencySnapshot()
			for _, wire := range s.Components {
				if wire.ID == e.ComponentID {
					if wire.StartupBuildBound != (kind == "bound") || wire.ProtocolEvidence == "verified" {
						t.Fatalf("%+v", wire)
					}
					if (kind == "report" || kind == "manifest" || kind == "binary") && wire.Integrity != "stale" {
						t.Fatalf("not stale: %+v", wire)
					}
				}
			}
		})
	}
}
