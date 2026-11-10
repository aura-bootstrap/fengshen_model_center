package components

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type Evidence struct {
	// Manifest is declaration-only metadata, not delivery or runtime verification.
	// Populate it only after strict parsing and controlled layout identity checks.
	Manifest         *Manifest      `json:"manifest,omitempty"`
	ComponentID      string         `json:"component_id"`
	Version          string         `json:"version"`
	Packaging        string         `json:"packaging"`
	Platform         string         `json:"platform"`
	FilesState       string         `json:"files_state"`
	Integrity        string         `json:"integrity"`
	StartupTest      string         `json:"startup_test"`
	CleanEnvironment string         `json:"clean_environment"`
	StartupReport    *StartupReport `json:"startup_report,omitempty"`
	ProtocolEvidence string         `json:"protocol_evidence"`
	Inference        string         `json:"inference"`
	Diagnostics      []string       `json:"diagnostics"`
	ManifestSHA256   string         `json:"manifest_sha256,omitempty"`
	CheckedAt        time.Time      `json:"checked_at,omitempty"`
}
type cached struct {
	manifest string
	entries  []safefs.Entry
	evidence Evidence
}
type Manager struct {
	root     string
	mu       sync.Mutex
	verified map[string]cached
	reports  map[string]cached
	// Deterministic package tests can arrange cancellation/replacement between reads.
	afterRead func()
}

// New expects the controlled components directory, not the project root.
func New(root string) *Manager {
	return &Manager{root: root, verified: map[string]cached{}, reports: map[string]cached{}}
}
func missing(id string) Evidence {
	return Evidence{ComponentID: id, Packaging: "pending_build", Platform: "not_checked", FilesState: "missing", Integrity: "not_checked", StartupTest: "not_tested", CleanEnvironment: "not_verified", ProtocolEvidence: "not_declared", Inference: "not_tested", Diagnostics: []string{"component_unbuilt: 未交付二进制组件；不执行源码或开发 venv"}}
}
func same(a, b safefs.Entry) bool {
	return a.Identity == b.Identity && a.Size == b.Size && a.Modified.Equal(b.Modified)
}
func open(s *safefs.Scope, rel string) (*os.File, safefs.Entry, error) {
	f, a, e := s.OpenFile(rel)
	if e == nil && a.LinkKind != "none" {
		f.Close()
		return nil, a, fmt.Errorf("component links are forbidden")
	}
	return f, a, e
}
func (m *Manager) scope() (*safefs.Scope, error) {
	st, e := os.Lstat(m.root)
	if e != nil {
		return nil, e
	}
	if !ordinaryDirectory(st) {
		return nil, fmt.Errorf("component root must be an ordinary directory")
	}
	s, err := safefs.Open(m.root)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(m.root)
	if err != nil || !strings.EqualFold(filepath.Clean(s.Path()), filepath.Clean(absolute)) {
		s.Close()
		return nil, fmt.Errorf("linked component root refused")
	}
	return s, nil
}
func (m *Manager) inspect(s *safefs.Scope, id, version string) (Evidence, Manifest, []safefs.Entry, error) {
	e := missing(id)
	e.Version = version
	var manifest Manifest
	prefix := id + "/" + version + "/"
	f, a, err := open(s, prefix+"manifest.json")
	if err != nil {
		return e, manifest, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	st, statErr := f.Stat()
	identity, idErr := safefs.Identity(f)
	closeErr := f.Close()
	g, after, reopenErr := open(s, prefix+"manifest.json")
	if g != nil {
		g.Close()
	}
	if err == nil && (statErr != nil || idErr != nil || closeErr != nil || reopenErr != nil || !same(a, after) || st.Size() != a.Size || !st.ModTime().Equal(a.Modified) || identity != a.Identity) {
		err = fmt.Errorf("manifest changed during read")
	}
	if err != nil {
		return e, manifest, nil, err
	}
	manifest, err = ParseManifest(strings.NewReader(string(b)))
	if err != nil {
		e.Packaging = "invalid_manifest"
		e.Diagnostics = []string{err.Error()}
		return e, manifest, nil, err
	}
	if manifest.ComponentID != id || manifest.Version != version {
		err = fmt.Errorf("manifest identity does not match controlled layout")
		e.Packaging = "invalid_manifest"
		e.Diagnostics = []string{err.Error()}
		return e, manifest, nil, err
	}
	sum := sha256.Sum256(b)
	e.ManifestSHA256 = hex.EncodeToString(sum[:])
	e.Manifest = &manifest
	e.Packaging = manifest.Packaging
	e.Diagnostics = []string{"本地清单仅证明一致性，不代表发行方认证、可信供应链或推理可用"}
	e.Platform = "matched"
	if manifest.OS != runtime.GOOS || manifest.Arch != runtime.GOARCH {
		e.Platform = "mismatch"
	}
	e.FilesState = "complete"
	entries := []safefs.Entry{a}
	for _, item := range manifest.Files {
		g, a, err := open(s, prefix+item.Path)
		if err != nil {
			e.FilesState = "incomplete"
			e.Diagnostics = append(e.Diagnostics, "component_file_missing: "+item.Path)
			entries = append(entries, a)
			continue
		}
		g.Close()
		entries = append(entries, a)
		n, _ := strconv.ParseInt(item.Bytes, 10, 64)
		if a.Size <= 0 || a.Size != n {
			e.FilesState = "incomplete"
			e.Diagnostics = append(e.Diagnostics, "component_file_size: "+item.Path)
		}
	}
	if len(manifest.Protocols) > 0 {
		e.ProtocolEvidence = "declared_not_tested"
	}
	if id == "whisper-stream" {
		protocol, flag := false, false
		for _, p := range manifest.Protocols {
			protocol = protocol || p == "fengshen.whispercpp.stream/v1"
			flag = flag || p == "--fengshen-stream-v1"
		}
		if !protocol || !flag {
			e.ProtocolEvidence = "missing_required_declaration"
			e.Diagnostics = append(e.Diagnostics, "批处理 CLI 不能代替流式组件")
		}
	}
	m.readStartupReport(s, prefix, manifest, &e, &entries)
	return e, manifest, entries, nil
}
func (m *Manager) List() []Evidence {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Evidence{}
	s, err := m.scope()
	if err != nil {
		for _, id := range IDs {
			e := missing(id)
			e.Diagnostics = append(e.Diagnostics, err.Error())
			out = append(out, e)
		}
		return out
	}
	defer s.Close()
	// Enumeration is bounded and only discovers version names; file access remains scoped.
	r, err := os.OpenRoot(m.root)
	if err != nil {
		for _, id := range IDs {
			out = append(out, missing(id))
		}
		return out
	}
	defer r.Close()
	manifests := map[string]Manifest{}
	indices := map[string]int{}
	for _, id := range IDs {
		// Never enumerate a linked component directory, even if its target is local.
		st, statErr := r.Lstat(id)
		if statErr != nil || !ordinaryDirectory(st) {
			out = append(out, missing(id))
			continue
		}
		d, err := r.Open(id)
		if err != nil {
			out = append(out, missing(id))
			continue
		}
		opened, openErr := d.Stat()
		current, currentErr := r.Lstat(id)
		if openErr != nil || currentErr != nil || !ordinaryDirectory(current) || !os.SameFile(st, opened) || !os.SameFile(current, opened) {
			d.Close()
			out = append(out, missing(id))
			continue
		}
		versions, err := d.ReadDir(65)
		d.Close()
		if len(versions) > 64 {
			e := missing(id)
			e.Diagnostics = append(e.Diagnostics, "component version limit exceeded")
			out = append(out, e)
			continue
		}
		if err != nil && err != io.EOF {
			e := missing(id)
			e.Diagnostics = append(e.Diagnostics, err.Error())
			out = append(out, e)
			continue
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i].Name() < versions[j].Name() })
		count := 0
		for _, v := range versions {
			vst, verr := r.Lstat(id + "/" + v.Name())
			if !identifier(v.Name()) || verr != nil || !ordinaryDirectory(vst) {
				continue
			}
			e, manifest, entries, err := m.inspect(s, id, v.Name())
			count++
			key := id + "/" + v.Name()
			if err == nil {
				manifests[key] = manifest
				if c, ok := m.verified[key]; ok {
					if c.manifest == e.ManifestSHA256 && len(c.entries) == len(entries) {
						valid := true
						for i := range entries {
							valid = valid && same(c.entries[i], entries[i])
						}
						if valid {
							e.Integrity = c.evidence.Integrity
							e.CheckedAt = c.evidence.CheckedAt
						} else {
							e.Integrity = "stale"
							if e.StartupReport != nil {
								e.StartupTest = "stale"
								e.StartupReport = nil
								e.CleanEnvironment = "not_verified"
							}
							delete(m.verified, key)
						}
					} else {
						e.Integrity = "stale"
						if e.StartupReport != nil {
							e.StartupTest = "stale"
							e.StartupReport = nil
							e.CleanEnvironment = "not_verified"
						}
						delete(m.verified, key)
					}
				}
			} else if _, ok := m.verified[key]; ok {
				e.Integrity = "stale"
				if e.StartupReport != nil {
					e.StartupTest = "stale"
					e.StartupReport = nil
					e.CleanEnvironment = "not_verified"
				}
				delete(m.verified, key)
			}
			if err != nil {
				if _, had := m.reports[key]; had {
					e.StartupTest = "stale"
					e.StartupReport = nil
				}
			}
			indices[key] = len(out)
			out = append(out, e)
		}
		if count == 0 {
			out = append(out, missing(id))
		}
	}
	// Reject cycles across manifests, and disclose unresolved binary dependencies.
	for key, manifest := range manifests {
		active := map[string]bool{}
		done := map[string]bool{}
		var visit func(string) bool
		visit = func(k string) bool {
			if done[k] {
				return false
			}
			if active[k] {
				return true
			}
			active[k] = true
			defer delete(active, k)
			for _, d := range manifests[k].RuntimeDependencies {
				if visit(d.ComponentID + "/" + d.Version) {
					return true
				}
			}
			done[k] = true
			return false
		}
		i := indices[key]
		if visit(key) {
			out[i].FilesState = "dependency_cycle"
			out[i].Diagnostics = append(out[i].Diagnostics, "component dependency cycle")
		}
		for _, d := range manifest.RuntimeDependencies {
			if _, ok := manifests[d.ComponentID+"/"+d.Version]; !ok {
				out[i].FilesState = "dependency_missing"
				out[i].Diagnostics = append(out[i].Diagnostics, "missing dependency: "+d.ComponentID+"/"+d.Version)
			}
		}
	}
	return out
}
func (m *Manager) Verify(ctx context.Context, id, version string) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		e := missing(id)
		e.Version = version
		e.Integrity = "cancelled"
		return e, err
	}
	for _, observed := range m.List() {
		if observed.ComponentID == id && observed.Version == version && (observed.FilesState == "dependency_cycle" || observed.FilesState == "dependency_missing") {
			return observed, fmt.Errorf("component dependency admission failed")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := missing(id)
	e.Version = version
	if !identifier(id) || !identifier(version) {
		return e, fmt.Errorf("invalid component target")
	}
	known := false
	for _, v := range IDs {
		known = known || id == v
	}
	if !known {
		return e, fmt.Errorf("unknown component")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	key := id + "/" + version
	delete(m.verified, key)
	s, err := m.scope()
	if err != nil {
		return e, err
	}
	defer s.Close()
	e, manifest, before, err := m.inspect(s, id, version)
	if err != nil {
		return e, err
	}
	e.Integrity = "failed"
	if e.FilesState != "complete" {
		return e, fmt.Errorf("component files incomplete")
	}
	buf := make([]byte, 1024*1024)
	for _, item := range manifest.Files {
		if err = ctx.Err(); err != nil {
			e.Integrity = "cancelled"
			return e, err
		}
		f, a, err := open(s, key+"/"+item.Path)
		if err != nil {
			return e, err
		}
		h := sha256.New()
		var n int64
		for {
			if err = ctx.Err(); err != nil {
				break
			}
			var count int
			count, err = f.Read(buf)
			if count > 0 {
				h.Write(buf[:count])
				n += int64(count)
				if m.afterRead != nil {
					m.afterRead()
				}
			}
			if err != nil {
				break
			}
		}
		st, statErr := f.Stat()
		identity, idErr := safefs.Identity(f)
		closeErr := f.Close()
		if err != io.EOF || statErr != nil || idErr != nil || closeErr != nil {
			if ctx.Err() != nil {
				e.Integrity = "cancelled"
				return e, ctx.Err()
			}
			return e, fmt.Errorf("component read failed: %v", err)
		}
		g, after, err := open(s, key+"/"+item.Path)
		if g != nil {
			g.Close()
		}
		if err != nil || !same(a, after) || identity != a.Identity || st.Size() != a.Size || !st.ModTime().Equal(a.Modified) || n != a.Size {
			e.Integrity = "stale"
			if e.StartupReport != nil {
				e.StartupTest = "stale"
				e.StartupReport = nil
				e.CleanEnvironment = "not_verified"
			}
			return e, fmt.Errorf("component file changed")
		}
		if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(item.SHA256) {
			e.Integrity = "mismatch"
			return e, fmt.Errorf("component_hash_mismatch: %s", item.Path)
		}
	}
	final, _, after, err := m.inspect(s, id, version)
	if err != nil || final.ManifestSHA256 != e.ManifestSHA256 || len(before) != len(after) {
		e.Integrity = "stale"
		if e.StartupReport != nil {
			e.StartupTest = "stale"
			e.StartupReport = nil
			e.CleanEnvironment = "not_verified"
		}
		return e, fmt.Errorf("manifest/files changed")
	}
	for i := range before {
		if !same(before[i], after[i]) {
			e.Integrity = "stale"
			if e.StartupReport != nil {
				e.StartupTest = "stale"
				e.StartupReport = nil
				e.CleanEnvironment = "not_verified"
			}
			return e, fmt.Errorf("manifest/files changed")
		}
	}
	if err = ctx.Err(); err != nil {
		e.Integrity = "cancelled"
		return e, err
	}
	e.Integrity = "matched"
	e.CheckedAt = time.Now().UTC()
	m.verified[key] = cached{e.ManifestSHA256, after, e}
	return e, nil
}

// ComponentDirectory returns the documented controlled relative layout.
func ComponentDirectory(root, id, version string) string { return filepath.Join(root, id, version) }
