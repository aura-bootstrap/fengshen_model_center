package main

import (
	"context"
	"encoding/json"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"strings"
	"testing"
	"time"
)

func TestDashboardSnapshotAggregateWithoutScan(t *testing.T) {
	m := fixtureManager(t)
	m.beforeScan = func(context.Context) { t.Error("dashboard started scan") }
	m.beforeRead = func(context.Context) { t.Error("dashboard started verification") }
	v, err := m.dashboardSnapshot()
	if err != nil || string(v.Inventory) != "null" || string(v.LastCompleteInventory) != "null" {
		t.Fatal(v, err)
	}
	var state State
	var observed []components.Evidence
	if json.Unmarshal(v.State, &state) != nil || len(state.Catalog) != len(catalog()) || state.ProjectRoot != m.root || json.Unmarshal(v.Components, &observed) != nil || len(observed) != len(components.IDs) {
		t.Fatal("missing fields", string(v.State), string(v.Components))
	}
	current, complete := opaqueID(), opaqueID()
	m.current, m.lastComplete = current, complete
	m.inventories[current] = &Inventory{ID: current, Complete: false, Models: []ModelState{{Model: Model{ID: "seed-vc"}, Inference: "not_tested"}}}
	m.inventories[complete] = &Inventory{ID: complete, Complete: true, Stale: true}
	v, err = m.dashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var inv, last Inventory
	if json.Unmarshal(v.Inventory, &inv) != nil || inv.ID != current || inv.Complete || json.Unmarshal(v.LastCompleteInventory, &last) != nil || last.ID != complete || !last.Complete || !last.Stale {
		t.Fatal(string(v.Inventory), string(v.LastCompleteInventory))
	}
	b, _ := json.Marshal(v)
	for _, secret := range []string{"token", "loopback_address", "pipe_name", "read_only_header"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("credential field", secret)
		}
	}
	if len(m.operations) != 0 || m.active != "" || m.lastAttempt != "" {
		t.Fatal("dashboard started operation")
	}
}
func TestDashboardDoesNotWaitForComponentVerification(t *testing.T) {
	m := fixtureManager(t)
	m.mu.Lock()
	m.active = "active-verification"
	m.operations[m.active] = &Operation{ID: m.active, Kind: "verify-component", Status: "running"}
	m.mu.Unlock()
	m.componentMu.Lock()
	defer m.componentMu.Unlock()
	done := make(chan error, 1)
	go func() {
		v, err := m.dashboardSnapshot()
		if err == nil {
			var state State
			var evidence []components.Evidence
			if json.Unmarshal(v.State, &state) != nil || state.ActiveOperation == nil || state.ActiveOperation.Status != "running" || json.Unmarshal(v.Components, &evidence) != nil || len(evidence) != len(components.IDs) {
				err = context.Canceled
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("missing published evidence or active operation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dashboard waited for component verification")
	}
}

func TestDashboardComponentEvidenceAndOperationAreOneSnapshot(t *testing.T) {
	m := fixtureManager(t)
	m.componentMu.Lock()
	defer m.componentMu.Unlock()
	publish := func(status string) {
		m.mu.Lock()
		m.lastAttempt = "verification"
		m.componentSnapshot = []components.Evidence{{ComponentID: "model-center", Version: "v1", Integrity: status}}
		m.operations[m.lastAttempt] = &Operation{ID: m.lastAttempt, Status: status}
		m.mu.Unlock()
	}
	publish("not_checked")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; n < 200; n++ {
			publish("matched")
			publish("not_checked")
		}
	}()
	for n := 0; n < 200; n++ {
		v, err := m.dashboardSnapshot()
		var state State
		var evidence []components.Evidence
		if err != nil || json.Unmarshal(v.State, &state) != nil || json.Unmarshal(v.Components, &evidence) != nil || len(evidence) != 1 || state.LastAttempt == nil || state.LastAttempt.Status != evidence[0].Integrity {
			t.Fatal("mixed component and operation generations", string(v.State), string(v.Components), err)
		}
	}
	<-done
}

func TestReportSnapshotExistenceAndFreshness(t *testing.T) {
	m := fixtureManager(t)
	m.beforeScan = func(context.Context) { t.Error("report started scan") }
	m.beforeRead = func(context.Context) { t.Error("report started verification") }
	type row struct {
		ID        string `json:"id"`
		Exists    *bool  `json:"exists"`
		Inference string `json:"inference"`
	}
	check := func(present, missing, unknown int) []row {
		t.Helper()
		r, err := m.reportSnapshot()
		var rows []row
		if err != nil || json.Unmarshal(r.Models, &rows) != nil || len(rows) != 12 || r.Summary.Total != 12 || r.Summary.Present != present || r.Summary.Missing != missing || r.Summary.Unknown != unknown {
			t.Fatal(r, string(r.Models), err)
		}
		for _, row := range rows {
			if row.Inference != "not_tested" {
				t.Fatal("inference promoted")
			}
		}
		if len(m.operations) != 0 {
			t.Fatal("report started operation")
		}
		return rows
	}
	check(0, 0, 12)
	id := opaqueID()
	m.current = id
	inv := &Inventory{ID: id, RulesVersion: rulesVersion, RegistryRevision: m.registry.Snapshot().Revision, Complete: true, FinishedAt: time.Now().UTC()}
	for _, model := range catalog() {
		inv.Models = append(inv.Models, ModelState{Model: model, DiscoveryStatus: "missing", Inference: "not_tested"})
	}
	inv.Models[0].DiscoveryStatus = "found"
	inv.Models[0].IdentityStatus = "recognized_by_rule"
	inv.Models[0].CompositionStatus = "complete"
	inv.Models[1].DiscoveryStatus = "registered_identity_unconfirmed"
	m.inventories[id] = inv
	rows := check(1, 10, 1)
	if rows[0].Exists == nil || !*rows[0].Exists || rows[1].Exists != nil || rows[2].Exists == nil || *rows[2].Exists {
		t.Fatal(rows)
	}
	inv.Complete = false
	check(1, 0, 11)
	inv.Stale = true
	check(0, 0, 12)
	inv.Stale = false
	inv.RegistryRevision++
	check(0, 0, 12)
	inv.RegistryRevision--
	inv.FinishedAt = time.Now().Add(-6 * time.Minute)
	check(0, 0, 12)
	inv.FinishedAt = time.Now()
	inv.RulesVersion = "old"
	check(0, 0, 12)
}

func TestReportHTTPReadsCenterOwnedSnapshot(t *testing.T) {
	m := fixtureManager(t)
	h := newHandler(m, "secret", "127.0.0.1:18120", nil)
	w := httpRequest(t, h, "GET", "/api/report", "", 200)
	var report struct {
		Summary struct{ Total, Unknown int }
		Models  []json.RawMessage
	}
	if json.Unmarshal(w.Body.Bytes(), &report) != nil || report.Summary.Total != 12 || report.Summary.Unknown != 12 || len(report.Models) != 12 {
		t.Fatal(w.Body.String())
	}
	httpRequest(t, h, "GET", "/api/report?path=W:/", "", 400)
	httpRequest(t, h, "POST", "/api/report", "{}", 405)
	if len(m.operations) != 0 {
		t.Fatal("GET scanned files")
	}
}

func TestDashboardFilesFixedPagination(t *testing.T) {
	m := fixtureManager(t)
	id := opaqueID()
	files := make([]*ModelFile, 401)
	for n := range files {
		files[n] = &ModelFile{ID: opaqueID(), Path: "asset.bin", Bytes: "3"}
	}
	m.current = id
	m.inventories[id] = &Inventory{ID: id, Models: []ModelState{{Model: Model{ID: "seed-vc"}, files: files}}}
	cursor := ""
	for _, want := range []int{200, 200, 1} {
		v, err := m.modelFilesPage(id, "seed-vc", cursor)
		var got []ModelFile
		if err != nil || v.Schema != 1 || json.Unmarshal(v.Files, &got) != nil || len(got) != want {
			t.Fatal(v, err)
		}
		cursor = v.NextCursor
	}
	if cursor != "" || len(m.operations) != 0 {
		t.Fatal("pagination side effects")
	}
	if _, err := m.modelFilesPage(id, "unknown", ""); err == nil {
		t.Fatal("unknown model")
	}
}
