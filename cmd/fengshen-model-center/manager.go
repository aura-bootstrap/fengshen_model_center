package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimehttp"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	maxPageCursors         = 1024
	cursorLifetime         = 30 * time.Minute
	maxInventoryTombstones = 256
)

type accessScope struct{ scope *safefs.Scope }
type pageCursor struct {
	inventory, model string
	offset           int
	createdAt        time.Time
}
type Manager struct {
	root, componentRoot                        string
	projectScope                               *safefs.Scope
	manifest                                   modelmanifest.Manifest
	manifestPath                               string
	manifestRevision                           uint64
	manifestError                              string
	manifestFiles                              map[string]map[string]bool
	extraRanges                                []scanRange
	registrySnap                               registry.Snapshot
	componentSnapshot                          []components.Evidence
	mu                                         sync.Mutex
	componentMu                                sync.Mutex
	bindingStore                               *bindings.Store
	runtimeHandler                             *runtimehttp.Handler
	components                                 *components.Manager
	ctx                                        context.Context
	stop                                       context.CancelFunc
	wg                                         sync.WaitGroup
	closed                                     bool
	inventories                                map[string]*Inventory
	gone                                       map[string]bool
	goneOrder                                  []string
	operations                                 map[string]*Operation
	cursors                                    map[string]pageCursor
	active, current, lastComplete, lastAttempt string
	cancel                                     context.CancelFunc
	scanEntries, scanDepth                     int
	scanTimeout, verifyTimeout                 time.Duration
	beforeScan                                 func(context.Context)
	beforeRead                                 func(context.Context)
	afterRead                                  func(context.Context)
}

func NewManager(root, store string, manifest modelmanifest.Manifest) (*Manager, error) {
	return newManagerAt(root, store, manifest, "development")
}
func newManagerAt(root, store string, manifest modelmanifest.Manifest, kind string) (*Manager, error) {
	raw, _ := json.Marshal(manifest)
	return newManagerWithInstallRoot(root, store, manifest, modelmanifest.Revision(raw), "", "", kind, "")
}
func newManagerWithInstallRoot(root, store string, manifest modelmanifest.Manifest, revision uint64, manifestErr, manifestPath, kind, installRoot string) (*Manager, error) {
	if root == "" {
		var e error
		root, e = os.Getwd()
		if e != nil {
			return nil, e
		}
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return nil, failure("root_invalid", "项目根目录无效", 422)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, failure("root_invalid", "项目根目录不存在", 422)
	}
	projectScope, e := safefs.Open(root)
	if e != nil {
		return nil, failure("root_invalid", "无法锚定项目根目录: "+e.Error(), 422)
	}
	check, _, e := projectScope.OpenDirectory(".")
	if e != nil {
		projectScope.Close()
		return nil, failure("root_invalid", "项目根目录不是受限目录", 422)
	}
	check.Close()
	if installRoot == "" {
		installRoot = root
	}
	if e = safefs.ValidatePath(installRoot); e != nil {
		projectScope.Close()
		return nil, failure("root_invalid", "组件安装根目录无效", 422)
	}
	componentRoot := filepath.Join(installRoot, "model-center", "components")
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{root: root, componentRoot: componentRoot, projectScope: projectScope, manifest: manifest, manifestPath: manifestPath, manifestRevision: revision, manifestError: manifestErr, ctx: ctx, stop: stop, inventories: map[string]*Inventory{}, gone: map[string]bool{}, operations: map[string]*Operation{}, cursors: map[string]pageCursor{}, scanEntries: 100000, scanDepth: 64, scanTimeout: 10 * time.Minute, verifyTimeout: 30 * time.Minute, components: components.New(componentRoot)}
	m.registrySnap, m.manifestFiles = syntheticSnapshot(manifest, revision, manifestErr)
	m.bindingStore, e = bindings.Open(filepath.Join(store, "execution-bindings"))
	if e != nil {
		m.Close()
		return nil, e
	}
	m.componentSnapshot = cloneJSON(m.components.List())
	return m, nil
}

func syntheticSnapshot(manifest modelmanifest.Manifest, revision uint64, manifestErr string) (registry.Snapshot, map[string]map[string]bool) {
	snap := registry.Snapshot{SchemaVersion: 1, Revision: revision, Error: manifestErr, Registrations: map[string]registry.Registration{}}
	dirs := map[string]string{}
	files := map[string]map[string]bool{}
	for _, e := range manifest.Entries {
		if _, ok := findModel(e.ModelID); !ok {
			continue
		}
		p := e.FilePath()
		dir := filepath.Dir(p)
		if current, ok := dirs[e.ModelID]; !ok {
			dirs[e.ModelID] = dir
		} else if !sameScanPath(current, dir) {
			continue
		}
		if files[e.ModelID] == nil {
			files[e.ModelID] = map[string]bool{}
		}
		files[e.ModelID][filepath.Base(p)] = true
	}
	for id, dir := range dirs {
		reg, err := validateRegistration(id, dir)
		if err != nil {
			continue
		}
		snap.Registrations[id] = reg
	}
	return snap, files
}

func validProject(root string) bool {
	for _, p := range []string{"sidecar", "models"} {
		s, e := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
		if e != nil || !s.IsDir() {
			return false
		}
	}
	return true
}
func validateRegistration(id, path string) (registry.Registration, error) {
	model, ok := findModel(id)
	if !ok {
		return registry.Registration{}, failure("model_unknown", "未知模型目录项", 422)
	}
	s, e := safefs.Open(path)
	if e != nil {
		return registry.Registration{}, failure("path_invalid", "路径不可访问或不符合本机范围限制: "+e.Error(), 422)
	}
	defer s.Close()
	f, e := os.Open(s.Path())
	if e != nil {
		return registry.Registration{}, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return registry.Registration{}, e
	}
	identity, e := safefs.Identity(f)
	if e != nil {
		return registry.Registration{}, e
	}
	kind := "directory"
	if !st.IsDir() {
		kind = "file"
		if !allowed(model, s.Path()) {
			return registry.Registration{}, failure("unsupported_file_type", "该目录项不允许此文件类型", 422)
		}
	}
	return registry.Registration{InputPath: path, ResolvedPath: s.Path(), Kind: kind, Identity: identity, UpdatedAt: time.Now().UTC()}, nil
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.stop()
	m.mu.Unlock()
	var runtimeErr error
	if m.runtimeHandler != nil {
		runtimeErr = m.runtimeHandler.Close()
	}
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.inventories {
		closeInventory(i)
	}
	var bindingErr error
	if m.bindingStore != nil {
		bindingErr = m.bindingStore.Close()
	}
	return errors.Join(m.projectScope.Close(), bindingErr, runtimeErr)
}
func closeInventory(i *Inventory) {
	for _, s := range i.access {
		s.scope.Close()
	}
}
func cloneJSON[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

// stateLocked requires mu, keeping dashboard inventory references consistent.
func (m *Manager) stateLocked() State {
	m.collect()
	snapshot := m.registrySnap
	s := State{StorageError: snapshot.Error, Catalog: catalog(), Registry: snapshot, CurrentInventory: m.current, LastCompleteInventory: m.lastComplete, ProjectRoot: m.root, Capabilities: capabilities()}
	if o := m.operations[m.active]; o != nil {
		v := cloneJSON(*o)
		s.ActiveOperation = &v
	}
	if o := m.operations[m.lastAttempt]; o != nil {
		v := cloneJSON(*o)
		s.LastAttempt = &v
	}
	return s
}
func (m *Manager) inventoryMissing(id string) error {
	if m.gone[id] {
		return failure("inventory_gone", "盘点已回收", 410)
	}
	return failure("inventory_unknown", "盘点不存在", 404)
}
func (m *Manager) Inventory(id string) (Inventory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collect()
	i := m.inventories[id]
	if i == nil {
		return Inventory{}, m.inventoryMissing(id)
	}
	return cloneJSON(*i), nil
}
func (m *Manager) Files(id, model, cursor string, limit int) ([]ModelFile, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collect()
	i := m.inventories[id]
	if i == nil {
		return nil, "", m.inventoryMissing(id)
	}
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return nil, "", failure("request_invalid", "分页条数必须为 1–1000", 422)
	}
	start := 0
	if cursor != "" {
		p, ok := m.cursors[cursor]
		if !ok || p.inventory != id || p.model != model {
			return nil, "", failure("cursor_invalid", "分页游标无效或已过期", 410)
		}
		start = p.offset
	}
	for _, s := range i.Models {
		if s.ID != model {
			continue
		}
		if start < 0 || start > len(s.files) {
			return nil, "", failure("cursor_invalid", "分页游标无效或已过期", 410)
		}
		end := start + limit
		if end > len(s.files) {
			end = len(s.files)
		}
		out := make([]ModelFile, 0, end-start)
		for _, f := range s.files[start:end] {
			out = append(out, cloneJSON(*f))
		}
		next := ""
		if end < len(s.files) {
			next = m.cacheCursor(pageCursor{inventory: id, model: model, offset: end})
		}
		return out, next, nil
	}
	return nil, "", failure("model_unknown", "未知模型", 404)
}

// cacheCursor reuses page boundaries and evicts the oldest token at the hard cap.
// Callers hold mu; cursor expiry never requires filesystem access.
func (m *Manager) cacheCursor(p pageCursor) string {
	oldest := ""
	for id, existing := range m.cursors {
		if existing.inventory == p.inventory && existing.model == p.model && existing.offset == p.offset {
			return id
		}
		if oldest == "" || existing.createdAt.Before(m.cursors[oldest].createdAt) {
			oldest = id
		}
	}
	if len(m.cursors) >= maxPageCursors {
		delete(m.cursors, oldest)
	}
	id := opaqueID()
	p.createdAt = time.Now().UTC()
	m.cursors[id] = p
	return id
}

func (m *Manager) begin(ctx context.Context, kind string, timeout time.Duration) (*Operation, context.Context, error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, e
	}
	if m.closed {
		return nil, nil, os.ErrClosed
	}
	if m.active != "" {
		return nil, nil, failure("operation_busy", "已有文件操作正在执行", 409)
	}
	m.collect()
	job, cancel := context.WithTimeout(m.ctx, timeout)
	m.cancel = cancel
	o := &Operation{ID: opaqueID(), Kind: kind, Status: "running", StartedAt: time.Now().UTC(), ProcessedBytes: "0", Hashes: []HashRecord{}, Diagnostics: []Diagnostic{}}
	m.operations[o.ID] = o
	m.active = o.ID
	m.lastAttempt = o.ID
	m.wg.Add(1)
	return o, job, nil
}
func (m *Manager) finish(id string, ctx context.Context, inv *Inventory, hashes []HashRecord, evidence *components.Evidence, observed []components.Evidence, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.operations[id]
	cancelled := ctx.Err() == context.Canceled || o.Status == "cancelling"
	o.Status = "completed"
	if err != nil {
		o.Status = "failed"
		o.Diagnostics = append(o.Diagnostics, Diagnostic{Code: "operation_failed", Message: err.Error()})
	}
	if ctx.Err() == context.DeadlineExceeded && inv == nil {
		o.Status = "failed"
		o.Diagnostics = append(o.Diagnostics, Diagnostic{Code: "verify_timeout", Message: "摘要校验超过30分钟限制"})
	}
	if cancelled {
		o.Status = "cancelled"
	}
	if inv != nil {
		if inv.RegistryRevision != m.manifestRevision {
			inv.Stale = true
			o.Status = "cancelled"
		}
		if o.Status == "cancelled" {
			inv.Complete = false
		}
		if !inv.Complete && o.Status == "completed" {
			o.Status = "partial"
		}
		m.inventories[inv.ID] = inv
		o.ResultID = inv.ID
		o.Diagnostics = append(o.Diagnostics, inv.Diagnostics...)
		if !inv.Stale && !m.closed {
			m.current = inv.ID
			if inv.Complete {
				m.lastComplete = inv.ID
			}
			m.invalidateHashes(inv)
		}
	}
	o.Hashes = hashes
	if evidence != nil {
		v := cloneJSON(*evidence)
		if o.Status == "cancelled" || ctx.Err() != nil {
			v.Integrity = "cancelled"
			for j := range observed {
				if observed[j].ComponentID == v.ComponentID && observed[j].Version == v.Version {
					observed[j].Integrity = "cancelled"
				}
			}
		}
		o.Evidence = &v
		m.componentSnapshot = cloneJSON(observed)
	}
	if o.Status == "cancelled" {
		for j := range o.Hashes {
			o.Hashes[j].Status = "cancelled"
			o.Hashes[j].SHA256 = ""
		}
		for _, inventory := range m.inventories {
			for _, model := range inventory.Models {
				for _, file := range model.files {
					if file.Hash != nil {
						for _, h := range o.Hashes {
							if h.FileID == file.ID {
								file.Hash = nil
							}
						}
					}
				}
			}
		}
	}
	if o.Kind == "verify" {
		for _, inventory := range m.inventories {
			refreshContentEqual(inventory)
		}
	}
	now := time.Now().UTC()
	o.FinishedAt = &now
	m.active = ""
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.collect()
}
func (m *Manager) Operation(id string) (*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collect()
	o := m.operations[id]
	if o == nil {
		return nil, failure("operation_gone", "操作不存在或已回收", 410)
	}
	v := cloneJSON(*o)
	return &v, nil
}
func (m *Manager) Cancel(id string) (*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.operations[id]
	if o == nil {
		return nil, failure("operation_gone", "操作不存在或已回收", 410)
	}
	if id == m.active {
		m.cancel()
		o.Status = "cancelling"
	}
	v := cloneJSON(*o)
	return &v, nil
}
func (m *Manager) collect() {
	now := time.Now()
	for id, p := range m.cursors {
		if now.Sub(p.createdAt) > cursorLifetime || m.inventories[p.inventory] == nil {
			delete(m.cursors, id)
		}
	}
	var done []*Operation
	for _, o := range m.operations {
		if o.FinishedAt != nil {
			done = append(done, o)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].FinishedAt.Before(*done[j].FinishedAt) })
	for n, o := range done {
		if len(done)-n > 32 || time.Since(*o.FinishedAt) > 30*time.Minute {
			delete(m.operations, o.ID)
		}
	}
	keep := map[string]bool{m.current: true, m.lastComplete: true}
	for _, o := range m.operations {
		keep[o.ResultID] = true
	}
	for id, i := range m.inventories {
		if !keep[id] {
			closeInventory(i)
			delete(m.inventories, id)
			m.gone[id] = true
			m.goneOrder = append(m.goneOrder, id)
			if len(m.goneOrder) > maxInventoryTombstones {
				delete(m.gone, m.goneOrder[0])
				m.goneOrder = m.goneOrder[1:]
			}
			for c, p := range m.cursors {
				if p.inventory == id {
					delete(m.cursors, c)
				}
			}
		}
	}
}
func (m *Manager) Components() []components.Evidence {
	// Serialize observation through publication so an older GET cannot overwrite
	// the evidence published by a concurrently completing verification.
	m.componentMu.Lock()
	defer m.componentMu.Unlock()
	observed := m.components.List()
	m.mu.Lock()
	m.componentSnapshot = cloneJSON(observed)
	m.mu.Unlock()
	return observed
}
func (m *Manager) StartVerifyComponent(ctx context.Context, id, version string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, job, e := m.begin(ctx, "verify-component", m.verifyTimeout)
	if e != nil {
		return Operation{}, e
	}
	go func() {
		defer m.wg.Done()
		m.componentMu.Lock()
		defer m.componentMu.Unlock()
		evidence, err := m.components.Verify(job, id, version)
		observed := m.components.List()
		for j := range observed {
			if observed[j].ComponentID == evidence.ComponentID && observed[j].Version == evidence.Version && observed[j].ManifestSHA256 == evidence.ManifestSHA256 && observed[j].Integrity != "stale" {
				observed[j] = cloneJSON(evidence)
			}
		}
		m.finish(o.ID, job, nil, nil, &evidence, observed, err)
	}()
	return cloneJSON(*o), nil
}
