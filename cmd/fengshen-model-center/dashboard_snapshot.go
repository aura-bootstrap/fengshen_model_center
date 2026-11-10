package main

import (
	"encoding/json"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"time"
)

// dashboardSnapshot aggregates existing inventory and bounded component metadata.
// It never starts a model scan, verification operation or inference process.
func (m *Manager) dashboardSnapshot() (integration.Dashboard, error) {
	if m.componentMu.TryLock() {
		observed := m.components.List()
		m.mu.Lock()
		m.componentSnapshot = cloneJSON(observed)
		m.mu.Unlock()
		m.componentMu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.stateLocked()
	v := integration.Dashboard{Schema: 1}
	fields := []struct {
		dst   *json.RawMessage
		value any
	}{
		{&v.State, state},
		{&v.Inventory, m.inventories[state.CurrentInventory]},
		{&v.LastCompleteInventory, m.inventories[state.LastCompleteInventory]},
		{&v.Components, m.componentSnapshot},
	}
	for _, f := range fields {
		b, err := json.Marshal(f.value)
		if err != nil {
			return integration.Dashboard{}, err
		}
		*f.dst = b
	}
	return v, nil
}
func (m *Manager) reportSnapshot() (integration.Report, error) {
	dashboard, err := m.dashboardSnapshot()
	if err != nil {
		return integration.Report{}, err
	}
	now := time.Now().UTC()
	var state State
	var inv *Inventory
	if err := json.Unmarshal(dashboard.State, &state); err != nil {
		return integration.Report{}, err
	}
	if err := json.Unmarshal(dashboard.Inventory, &inv); err != nil {
		return integration.Report{}, err
	}
	report := integration.Report{Dashboard: dashboard, ObservedAt: now, RegistryRevision: state.Registry.Revision, Stale: true}
	observed := map[string]ModelState{}
	if inv != nil {
		report.InventoryID, report.Complete = inv.ID, inv.Complete
		report.Stale = state.StorageError != "" || inv.Stale || inv.RegistryRevision != state.Registry.Revision || inv.RulesVersion != rulesVersion || inv.FinishedAt.IsZero() || inv.FinishedAt.After(now) || now.Sub(inv.FinishedAt) > 5*time.Minute
		for _, model := range inv.Models {
			observed[model.ID] = model
		}
	}
	rows := []struct {
		ModelState
		Exists *bool `json:"exists"`
	}{}
	for _, model := range state.Catalog {
		row := ModelState{Model: model, DiscoveryStatus: "not_scanned", IdentityStatus: "not_checked", CompositionStatus: "not_checked", DependencyStatus: "not_checked", VerificationStatus: "not_checked", Inference: "not_tested", Groups: map[string]string{}}
		if found, ok := observed[model.ID]; ok {
			row = found
			row.Model = model
		}
		var exists *bool
		if !report.Stale {
			if row.DiscoveryStatus == "found" {
				present := true
				exists = &present
			} else if report.Complete && row.DiscoveryStatus == "missing" {
				present := false
				exists = &present
			}
		}
		report.Summary.Total++
		if exists == nil {
			report.Summary.Unknown++
		} else if *exists {
			report.Summary.Present++
		} else {
			report.Summary.Missing++
		}
		if !report.Stale && row.IdentityStatus == "recognized_by_rule" {
			report.Summary.Recognized++
		}
		if !report.Stale && row.CompositionStatus == "complete" {
			report.Summary.CompositionComplete++
		}
		rows = append(rows, struct {
			ModelState
			Exists *bool `json:"exists"`
		}{row, exists})
	}
	report.Models, err = json.Marshal(rows)
	return report, err
}

func (m *Manager) modelFilesPage(inventoryID, modelID, cursor string) (integration.FilesPage, error) {
	files, next, err := m.Files(inventoryID, modelID, cursor, 200)
	if err != nil {
		return integration.FilesPage{}, err
	}
	b, err := json.Marshal(files)
	if err != nil {
		return integration.FilesPage{}, err
	}
	return integration.FilesPage{Schema: 1, Files: b, NextCursor: next}, nil
}
