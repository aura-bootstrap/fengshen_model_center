package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

func (m *Manager) dependencySnapshot() integration.DependencySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	s := integration.DependencySnapshot{Schema: 1, ObservedAt: now, RegistryRevision: m.registry.Snapshot().Revision, Stale: true, Components: []integration.Evidence{}, Models: []integration.Evidence{}, Diagnostics: []string{}}
	// Exactly one row per fixed ID; neither ordering nor version names select a build.
	for _, id := range components.IDs {
		wire := integration.Evidence{ID: id, Packaging: "not_observed", Files: "unknown", Integrity: "not_checked", StartupTest: "not_tested", Optional: id != "model-center", Inference: "not_tested"}
		var selected components.Evidence
		count := 0
		for _, e := range m.componentSnapshot {
			if e.ComponentID == id {
				selected = e
				count++
			}
		}
		if count > 1 {
			s.Diagnostics = append(s.Diagnostics, "component_version_ambiguous")
		} else if count == 1 {
			e := selected
			wire.Packaging, wire.Platform, wire.Files = e.Packaging, e.Platform, e.FilesState
			wire.Integrity, wire.StartupTest, wire.ProtocolEvidence = e.Integrity, e.StartupTest, e.ProtocolEvidence
			needsCurrent := e.ManifestSHA256 != "" || e.Integrity == "matched" || e.StartupReport != nil
			current := m.components != nil && m.components.CachedCurrent(e)
			if needsCurrent && !current {
				wire.Integrity, wire.StartupTest = "stale", "stale"
				s.Diagnostics = append(s.Diagnostics, "component_evidence_stale")
			}
			r := e.StartupReport
			wire.StartupBuildBound = current && e.Integrity == "matched" && e.StartupTest == "passed" && e.Platform == "matched" && e.FilesState == "complete" && r != nil && r.StartupTest == "passed" && r.ComponentID == e.ComponentID && r.Version == e.Version && e.ManifestSHA256 != "" && strings.EqualFold(r.ManifestSHA256, e.ManifestSHA256)
		}
		s.Components = append(s.Components, wire)
	}
	inv := m.inventories[m.current]
	if inv != nil {
		s.InventoryID, s.FinishedAt, s.Complete = inv.ID, inv.FinishedAt, inv.Complete
		s.Stale = inv.Stale || inv.RegistryRevision != s.RegistryRevision || inv.FinishedAt.IsZero() || inv.FinishedAt.After(now) || now.Sub(inv.FinishedAt) > 5*time.Minute
		for _, model := range inv.Models {
			nonempty := model.DiscoveryStatus == "found" && model.IdentityStatus == "recognized_by_rule" && model.CompositionStatus == "complete" && len(model.files) > 0
			for _, file := range model.files {
				if file == nil {
					nonempty = false
					continue
				}
				n, err := strconv.ParseInt(file.Bytes, 10, 64)
				if err != nil || n <= 0 || file.Evidence != "recognized_by_rule" {
					nonempty = false
				}
			}
			s.Models = append(s.Models, integration.Evidence{ID: model.ID, Discovery: model.DiscoveryStatus, Composition: model.CompositionStatus, Dependency: model.DependencyStatus, NonEmpty: nonempty, Optional: model.Optional, Inference: "not_tested"})
		}
		for _, diagnostic := range inv.Diagnostics {
			if len(s.Diagnostics) == 256 {
				break
			}
			// Only stable codes cross the IPC boundary, never messages or paths.
			s.Diagnostics = append(s.Diagnostics, diagnostic.Code)
		}
	} else {
		s.Diagnostics = append(s.Diagnostics, "inventory_not_available")
		for _, model := range catalog() {
			s.Models = append(s.Models, integration.Evidence{ID: model.ID, Discovery: "unknown", Composition: "unknown", Dependency: "unknown", Optional: model.Optional, Inference: "not_tested"})
		}
	}
	return s
}
