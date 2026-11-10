package main

import (
	"context"
	"errors"
	"github.com/aura-bootstrap/fengshen_model_center/registry"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"
)

type scanRange struct {
	path, kind   string
	models       []string
	registration *registry.Registration
}
type measuredFile struct {
	entry   safefs.Entry
	scopeID string
}

func (m *Manager) StartScan(ctx context.Context, revision uint64) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.registry.Snapshot()
	if snapshot.Revision != revision {
		return Operation{}, failure("registry_conflict", "登记版本已变化，请刷新", 409)
	}
	o, job, e := m.begin(ctx, "scan", m.scanTimeout)
	if e != nil {
		return Operation{}, e
	}
	go func() {
		defer m.wg.Done()
		if m.beforeScan != nil {
			m.beforeScan(job)
		}
		inv := m.scan(job, o.ID, snapshot)
		m.finish(o.ID, job, inv, nil, nil, nil, nil)
	}()
	return cloneJSON(*o), nil
}
func (m *Manager) ranges(s registry.Snapshot) []scanRange {
	var defaults []string
	if m.installed || len(m.roots) > 0 {
		defaults = append(defaults, m.roots...)
	} else {
		defaults = []string{filepath.Join(m.root, "sidecar", "seed-vc", "checkpoints"), filepath.Join(m.root, "models")}
		if m.torchRoot != "" {
			defaults = append(defaults, m.torchRoot)
		}
	}
	var ids []string
	for _, model := range catalog() {
		if _, registered := s.Registrations[model.ID]; !registered && model.Embedded == "" {
			ids = append(ids, model.ID)
		}
	}
	var out []scanRange
	for _, p := range defaults {
		out = append(out, scanRange{path: p, kind: "asset", models: append([]string(nil), ids...)})
	}
	for _, model := range catalog() {
		if r, ok := s.Registrations[model.ID]; ok {
			r := r
			out = append(out, scanRange{path: r.InputPath, kind: "registration", models: []string{model.ID}, registration: &r})
		} else if model.Embedded != "" && !m.installed {
			out = append(out, scanRange{path: filepath.Join(m.root, model.Embedded), kind: "asset", models: []string{model.ID}})
		}
	}
	if m.installed {
		for _, rel := range m.haarFiles {
			for _, model := range catalog() {
				if model.Embedded != "" && filepath.Base(model.Embedded) == filepath.Base(rel) {
					if _, registered := s.Registrations[model.ID]; !registered {
						out = append(out, scanRange{path: filepath.Join(m.root, filepath.FromSlash(rel)), kind: "asset", models: []string{model.ID}})
					}
				}
			}
		}
	} else {
		out = append(out, scanRange{path: filepath.Join(m.root, "sidecar", "venv"), kind: "runtime"})
	}
	// Visit parents before children so overlapping authorised roots can reuse metadata.
	sort.SliceStable(out, func(i, j int) bool { return len(filepath.Clean(out[i].path)) < len(filepath.Clean(out[j].path)) })
	return out
}
func (m *Manager) openScanRange(r scanRange) (*safefs.Scope, error) {
	if r.registration != nil {
		return safefs.Open(r.path)
	}
	if rel, err := filepath.Rel(m.root, r.path); err == nil && filepath.IsLocal(rel) {
		return m.projectScope.Derive(rel)
	}
	if m.torchRoot != "" && sameScanPath(m.torchRoot, r.path) {
		if m.homeError != nil {
			// A missing trusted anchor cannot establish a missing *cache* boundary.
			return nil, fmt.Errorf("%v: %w", m.homeError, safefs.ErrBoundary)
		}
		return m.homeScope.Derive(filepath.Join(".cache", "torch", "hub", "checkpoints"))
	}
	if m.installed {
		return nil, safefs.ErrBoundary
	}
	// Tests/embedders explicitly authorize custom external roots. Do not replace
	// them with their parent or home, or add the production defaults alongside them.
	return safefs.Open(r.path)
}

func sameScanPath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

func sourcePath(e safefs.Entry) string {
	if e.SourcePath != "" {
		return e.SourcePath
	}
	return e.Path
}

func fileKey(e safefs.Entry) string {
	if e.Identity != "" {
		return "id:" + e.Identity
	}
	return "path:" + filepath.Clean(e.Path)
}
func sumEntries(files map[string]safefs.Entry, complete, known bool) SizeMetric {
	var n int64
	quality := "file_identity"
	for _, e := range files {
		n += e.Size
		if e.Identity == "" {
			quality = "limited_path_only"
		}
	}
	v := metric(n, len(files), complete, known)
	v.IdentityQuality = quality
	return v
}
func (m *Manager) scan(ctx context.Context, operation string, snapshot registry.Snapshot) *Inventory {
	inv := &Inventory{ID: opaqueID(), RulesVersion: rulesVersion, RegistryRevision: snapshot.Revision, StartedAt: time.Now().UTC(), Complete: true, ProjectRoot: m.root, Models: []ModelState{}, Scopes: []ScopeResult{}, Diagnostics: []Diagnostic{}, RuntimeEnvironments: []RuntimeEnvironment{}, access: map[string]*accessScope{}}
	index := map[string]int{}
	perModel := map[string]map[string]*ModelFile{}
	knownModel := map[string]bool{}
	completeModel := map[string]bool{}
	modelGroups := map[string]map[string]map[string]bool{}
	for _, model := range catalog() {
		index[model.ID] = len(inv.Models)
		inv.Models = append(inv.Models, ModelState{Model: model, DiscoveryStatus: "missing", IdentityStatus: "unconfirmed", CompositionStatus: "missing", DependencyStatus: "not_applicable", VerificationStatus: "not_checked", Inference: "not_tested", Groups: map[string]string{}, files: []*ModelFile{}})
		perModel[model.ID] = map[string]*ModelFile{}
		modelGroups[model.ID] = map[string]map[string]bool{}
		completeModel[model.ID] = true
	}
	assets, runtimeFiles := map[string]safefs.Entry{}, map[string]safefs.Entry{}
	assetKnown, runtimeKnown := false, false
	assetComplete, runtimeComplete := true, true
	// An exact repeated root shares its one enumeration. Overlapping roots reuse observed
	// metadata and bind a separate scope, so registration never grants another model access.
	cached := map[string][]safefs.Entry{}
	cachedErr := map[string]error{}
	entries := 0
	diagnose := func(code, msg, scope string, ids []string) {
		inv.Complete = false
		inv.Diagnostics = append(inv.Diagnostics, Diagnostic{Code: code, Message: msg, ScopeID: scope})
		for _, id := range ids {
			completeModel[id] = false
		}
	}
	for _, r := range m.ranges(snapshot) {
		sr := ScopeResult{ID: opaqueID(), Path: r.path, Kind: r.kind, Status: "complete", Complete: true, ModelIDs: append([]string{}, r.models...)}
		var environment *RuntimeEnvironment
		if r.kind == "runtime" {
			rel := filepath.Join("bin", "python")
			if runtime.GOOS == "windows" {
				rel = filepath.Join("Scripts", "python.exe")
			}
			inv.RuntimeEnvironments = append(inv.RuntimeEnvironments, RuntimeEnvironment{ScopeID: sr.ID, Kind: "development_venv", InterpreterPath: filepath.Join(r.path, rel), InterpreterStatus: "not_checked", DeliveryStatus: "pending_build", Inference: "not_tested", Message: "仅检查历史开发环境文件元数据，不执行解释器或版本命令；文件存在不代表已交付二进制组件或推理可用。"})
			environment = &inv.RuntimeEnvironments[len(inv.RuntimeEnvironments)-1]
		}
		scope, e := m.openScanRange(r)
		if e != nil {
			sr.Status = "unavailable"
			sr.Complete = false
			if errors.Is(e, os.ErrNotExist) && r.registration == nil {
				sr.Status = "missing"
				sr.Complete = true
			} else {
				diagnose("scope_unavailable", "扫描范围不可访问: "+e.Error(), sr.ID, r.models)
				if r.kind == "runtime" {
					runtimeComplete = false
				} else {
					assetComplete = false
				}
			}
			if environment != nil {
				environment.InterpreterStatus = sr.Status
			}
			inv.Scopes = append(inv.Scopes, sr)
			continue
		}
		sr.Path = scope.Path()
		if r.registration != nil {
			current, err := validateRegistration(r.models[0], r.path)
			if err != nil || current.Identity != r.registration.Identity || current.ResolvedPath != r.registration.ResolvedPath {
				scope.Close()
				sr.Status = "unavailable"
				sr.Complete = false
				diagnose("registration_changed", "登记入口已变化，请重新登记", sr.ID, r.models)
				assetComplete = false
				inv.Scopes = append(inv.Scopes, sr)
				continue
			}
		}
		inv.access[sr.ID] = &accessScope{scope: scope}
		if r.kind == "runtime" {
			runtimeKnown = true
		} else {
			assetKnown = true
		}
		for _, id := range r.models {
			knownModel[id] = true
		}
		key := filepath.Clean(scope.Path())
		observed, seen := cached[key]
		walkErr := cachedErr[key]
		if !seen {
			for parent, prior := range cached {
				rel, err := filepath.Rel(parent, key)
				if err != nil || rel == "." || !filepath.IsLocal(rel) || cachedErr[parent] != nil {
					continue
				}
				for _, entry := range prior {
					r, err := filepath.Rel(key, sourcePath(entry))
					if err != nil || !filepath.IsLocal(r) {
						continue
					}
					if r == "." {
						if entry.IsDir {
							continue // Walk emits children, not the scope directory itself.
						}
						r = filepath.Base(key)
					}
					// Revalidate against the narrower root: a link allowed by the
					// parent must not borrow that parent's boundary for verification.
					var f *os.File
					if entry.IsDir {
						f, entry, err = scope.OpenDirectory(r)
					} else {
						f, entry, err = scope.OpenFile(r)
					}
					if f != nil {
						f.Close()
					}
					if err != nil {
						entry.Error = err.Error()
						walkErr = errors.Join(walkErr, err)
					}
					observed = append(observed, entry)
				}
				seen = true
				cached[key], cachedErr[key] = observed, walkErr
				break
			}
		}
		if !seen {
			if entries >= m.scanEntries {
				walkErr = safefs.ErrLimit
			} else {
				walkErr = scope.Walk(ctx, safefs.Limits{MaxEntries: m.scanEntries - entries, MaxDepth: m.scanDepth}, func(a safefs.Entry) error {
					entries++
					observed = append(observed, a)
					m.mu.Lock()
					if o := m.operations[operation]; o != nil {
						o.CheckedEntries = entries
					}
					m.mu.Unlock()
					return ctx.Err()
				})
			}
			cached[key] = observed
			cachedErr[key] = walkErr
		}
		if walkErr != nil {
			sr.Status = "incomplete"
			sr.Complete = false
			code := "scope_incomplete"
			if errors.Is(walkErr, safefs.ErrLimit) {
				code = "scan_limit"
			}
			if errors.Is(walkErr, context.DeadlineExceeded) {
				code = "scan_timeout"
			}
			if errors.Is(walkErr, context.Canceled) {
				code = "operation_cancelled"
			}
			diagnose(code, "扫描未完成: "+walkErr.Error(), sr.ID, r.models)
			if r.kind == "runtime" {
				runtimeComplete = false
			} else {
				assetComplete = false
			}
		}
		if environment != nil {
			if sr.Complete {
				environment.InterpreterStatus = "missing"
			}
			for _, a := range observed {
				if filepath.Clean(filepath.Join(r.path, a.Relative)) != filepath.Clean(environment.InterpreterPath) {
					continue
				}
				if a.Error != "" {
					environment.InterpreterStatus = "unavailable"
				} else if !a.IsDir {
					environment.InterpreterStatus = "found"
					if a.Size == 0 {
						environment.InterpreterStatus = "empty_file"
					}
					bytes := strconv.FormatInt(a.Size, 10)
					environment.InterpreterBytes = &bytes
				}
			}
		}
		for _, a := range observed {
			if a.IsDir || a.Error != "" {
				continue
			}
			k := fileKey(a)
			if r.kind == "runtime" {
				runtimeFiles[k] = a
				continue
			}
			assets[k] = a
			for _, id := range r.models {
				state := &inv.Models[index[id]]
				source := sourcePath(a)
				recognized := recognizes(state.Model, source)
				if !recognized && (r.registration == nil || !allowed(state.Model, source)) {
					continue
				}
				evidence := "registered_identity_unconfirmed"
				if recognized {
					evidence = "recognized_by_rule"
					group := filepath.Dir(source)
					if modelGroups[id][group] == nil {
						modelGroups[id][group] = map[string]bool{}
					}
					components := append([]string{}, state.Components...)
					for _, parts := range state.ComponentAlternatives {
						components = append(components, parts...)
					}
					if repo, parts := demucsHFComponents(state.Model); repo != "" && filepath.Ext(source) == ".safetensors" {
						components = parts
					}
					for _, pattern := range components {
						if match, _ := filepath.Match(pattern, filepath.Base(source)); match && a.Size > 0 {
							modelGroups[id][group][pattern] = true
						}
					}
				}
				if old := perModel[id][k]; old != nil {
					if old.Path != source && !contains(old.Aliases, source) {
						old.Aliases = append(old.Aliases, source)
					}
					if a.Path != old.Path && !contains(old.Aliases, a.Path) {
						old.Aliases = append(old.Aliases, a.Path)
					}
					if recognized && old.Evidence != "recognized_by_rule" {
						// Prefer a recognized source over an earlier unconfirmed alias,
						// keeping the matching scope/relative pair for restricted reads.
						previous := old.Path
						old.Path, old.Group = source, filepath.Dir(source)
						old.ScopeID, old.relative, old.LinkKind = sr.ID, a.Relative, a.LinkKind
						aliases := []string{}
						for _, path := range append(old.Aliases, previous) {
							if path != source && !contains(aliases, path) {
								aliases = append(aliases, path)
							}
						}
						old.Aliases = aliases
						old.Evidence = evidence
					}
					continue
				}
				aliases := []string{}
				if source != a.Path {
					aliases = append(aliases, a.Path)
				}
				f := &ModelFile{ID: opaqueID(), ScopeID: sr.ID, Path: source, Aliases: aliases, Bytes: strconv.FormatInt(a.Size, 10), Modified: a.Modified, Identity: a.Identity, LinkKind: a.LinkKind, Evidence: evidence, Group: filepath.Dir(source), relative: a.Relative, size: a.Size}
				perModel[id][k] = f
				state.files = append(state.files, f)
			}
		}
		inv.Scopes = append(inv.Scopes, sr)
	}
	weights, seedSuite := map[string]safefs.Entry{}, map[string]safefs.Entry{}
	for n := range inv.Models {
		s := &inv.Models[n]
		sort.Slice(s.files, func(i, j int) bool { return s.files[i].Path < s.files[j].Path })
		s.FileCount = len(s.files)
		identified := map[string]safefs.Entry{}
		groups := modelGroups[s.ID]
		empty := false
		candidate := false
		for _, f := range s.files {
			sort.Strings(f.Aliases)
			if f.size == 0 {
				empty = true
			}
			if f.Evidence != "recognized_by_rule" {
				candidate = true
				continue
			}
			a := safefs.Entry{Path: f.Path, Identity: f.Identity, Size: f.size, Modified: f.Modified}
			k := fileKey(a)
			identified[k] = a
			weights[k] = a
			if s.ID == "seed-vc" || s.ID == "seed-whisper" || s.ID == "campplus" || s.ID == "bigvgan" {
				seedSuite[k] = a
			}
		}
		known := knownModel[s.ID]
		if (s.ID == "local-text" || s.ID == "local-vision") && len(identified) == 0 {
			known = false
		}
		if candidate && len(identified) == 0 {
			known = false
		}
		s.Size = sumEntries(identified, completeModel[s.ID], known)
		if len(s.files) > 0 {
			s.DiscoveryStatus = "found"
			if len(identified) > 0 {
				s.IdentityStatus = "recognized_by_rule"
			} else {
				s.DiscoveryStatus = "registered_identity_unconfirmed"
			}
			if empty {
				s.DiscoveryStatus = "empty_file"
			}
		}
		for _, scope := range inv.Scopes {
			if contains(scope.ModelIDs, s.ID) && !scope.Complete {
				s.DiscoveryStatus = "scan_incomplete"
				if scope.Status == "unavailable" {
					s.DiscoveryStatus = "unavailable"
				}
			}
		}
		for g, parts := range groups {
			status := "incomplete"
			complete := len(s.Components) > 0
			for _, part := range s.Components {
				complete = complete && parts[part]
			}
			for _, alternatives := range s.ComponentAlternatives {
				alternativeComplete := len(alternatives) > 0
				for _, part := range alternatives {
					alternativeComplete = alternativeComplete && parts[part]
				}
				complete = complete || alternativeComplete
			}
			if repo, alternatives := demucsHFComponents(s.Model); repo != "" {
				hfComplete := true
				for _, part := range alternatives {
					hfComplete = hfComplete && parts[part]
				}
				complete = complete || hfComplete
			}
			if complete {
				status = "complete"
				s.CompositionStatus = "complete"
			}
			s.Groups[g] = status
		}
		if len(s.files) > 0 && s.CompositionStatus != "complete" {
			s.CompositionStatus = "incomplete"
		}
	}
	for n := range inv.Models {
		s := &inv.Models[n]
		if len(s.Dependencies) > 0 {
			s.DependencyStatus = "files_found"
			for _, id := range s.Dependencies {
				d := inv.Models[index[id]]
				if d.CompositionStatus != "complete" {
					s.DependencyStatus = "incomplete"
				}
			}
		}
	}
	// Candidate evidence never merges ordinary copies. O(n) grouping avoids quadratic
	// work at the 100000-entry boundary.
	names := map[string][]*ModelFile{}
	sizes := map[string][]*ModelFile{}
	for _, s := range inv.Models {
		for _, f := range s.files {
			names[filepath.Base(f.Path)] = append(names[filepath.Base(f.Path)], f)
			sizes[f.Bytes] = append(sizes[f.Bytes], f)
		}
	}
	for _, groups := range []map[string][]*ModelFile{names, sizes} {
		for _, fs := range groups {
			unique := map[string]bool{}
			for _, f := range fs {
				unique[fileKey(safefs.Entry{Path: f.Path, Identity: f.Identity})] = true
			}
			if len(unique) > 1 {
				for _, f := range fs {
					f.DuplicateCandidate = true
				}
			}
		}
	}
	if ctx.Err() != nil {
		inv.Complete = false
		assetComplete = false
		runtimeComplete = false
	}
	inv.WeightSize = sumEntries(weights, inv.Complete, assetKnown)
	inv.AssetSize = sumEntries(assets, assetComplete, assetKnown)
	inv.RuntimeSize = sumEntries(runtimeFiles, runtimeComplete, runtimeKnown)
	seedKnown := true
	for _, id := range []string{"seed-vc", "seed-whisper", "campplus", "bigvgan"} {
		seedKnown = seedKnown && inv.Models[index[id]].Size.Bytes != nil
	}
	inv.SeedSuiteSize = sumEntries(seedSuite, inv.Complete, seedKnown)
	inv.FinishedAt = time.Now().UTC()
	return inv
}
func contains(xs []string, s string) bool {
	for _, v := range xs {
		if v == s {
			return true
		}
	}
	return false
}

// invalidateHashes runs under the manager lock. Historical operation evidence remains
// available, but is never presented as the current fingerprint after metadata changes.
func (m *Manager) invalidateHashes(inv *Inventory) {
	current := map[string]*ModelFile{}
	for _, s := range inv.Models {
		for _, f := range s.files {
			current[s.ID+"\x00"+f.Path] = f
		}
	}
	for _, old := range m.inventories {
		if old.ID == inv.ID {
			continue
		}
		for _, s := range old.Models {
			for _, f := range s.files {
				if f.Hash == nil || f.Hash.Status != "fingerprint_computed" {
					continue
				}
				now := current[s.ID+"\x00"+f.Path]
				if now == nil || now.Identity != f.Identity || now.Bytes != f.Bytes || !now.Modified.Equal(f.Modified) {
					f.Hash.Status = "stale"
					for _, o := range m.operations {
						for j := range o.Hashes {
							if o.Hashes[j].FileID == f.ID {
								o.Hashes[j].Status = "stale"
							}
						}
					}
				} else {
					h := *f.Hash
					h.FileID = now.ID
					now.Hash = &h
				}
			}
		}
		refreshContentEqual(old)
	}
	refreshContentEqual(inv)
}
