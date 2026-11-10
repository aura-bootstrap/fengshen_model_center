package integration

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

// HostConfig is supplied after the real HTTP listener and registry lock exist.
// Build is a stable build identifier, NOT the self-referential EXE digest.
// Read callbacks must return existing evidence, never scan or launch processes.
type HostConfig struct {
	Config              Config
	Address             string
	Build               string
	Health              func() Health
	Dependencies        func() DependencySnapshot
	Dashboard           func() (Dashboard, error)
	Report              func() (Report, error)
	ModelFiles          func(inventoryID, modelID, cursor string) (FilesPage, error)
	RuntimeCapabilities func() (RuntimeCapabilities, error)
}
type Host struct {
	mu     sync.Mutex
	cfg    HostConfig
	desc   Descriptor
	now    func() time.Time
	lock   io.Closer
	closed bool
}

func NewHost(cfg HostConfig) (*Host, error) {
	if e := platformSupported(); e != nil {
		return nil, e
	}
	if _, e := canonical(cfg.Address); e != nil {
		return nil, e
	}
	if cfg.Build == "" || !safeCode(cfg.Build) {
		return nil, errors.New("center_build_invalid")
	}
	if e := validateDirectories(cfg.Config); e != nil {
		return nil, e
	}
	return &Host{cfg: cfg, now: time.Now}, nil
}

// Register acquires a private instance lock and publishes an atomic, non-secret
// descriptor for local discovery.
func (h *Host) Register() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.lock != nil {
		return errors.New("center_host_state_invalid")
	}
	if e := safefs.SecureDir(h.cfg.Config.DataDir); e != nil {
		return e
	}
	lock, e := safefs.Lock(filepath.Join(h.cfg.Config.DataDir, "instance.lock"))
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	root, data, e := directoryIdentities(h.cfg.Config)
	if e != nil {
		return e
	}
	p, e := queryProcess(uint32(os.Getpid()))
	if e != nil {
		return e
	}
	id, e := randomID()
	if e != nil {
		return e
	}
	h.desc = Descriptor{Schema: 1, InstanceID: id, PID: p.PID, ProcessStartedAt: p.Created, LoopbackAddress: h.cfg.Address, Build: h.cfg.Build, RootKind: h.cfg.Config.RootKind, RootIdentity: root, DataIdentity: data}
	b, e := json.Marshal(h.desc)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(h.cfg.Config.DataDir, ".descriptor-*")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	done := descriptorPhase("write")
	_, e = f.Write(b)
	done()
	if e == nil {
		done = descriptorPhase("sync")
		e = f.Sync()
		done()
	}
	done = descriptorPhase("close")
	ce := f.Close()
	done()
	if e == nil {
		e = ce
	}
	if e == nil {
		done = descriptorPhase("publish")
		e = safefs.Replace(temp, filepath.Join(h.cfg.Config.DataDir, DescriptorName))
		done()
	}
	if e != nil {
		return e
	}
	h.lock = lock
	ok = true
	return nil
}

// descriptorPhase logs only fixed operation names and timing, never operation data.
// done means the call returned, not that it succeeded.
func descriptorPhase(phase string) func() {
	switch phase {
	case "write", "sync", "close", "publish":
	default:
		return func() {}
	}
	log.Printf("center_descriptor phase=%s event=start elapsed_ms=0", phase)
	started := time.Now()
	return func() {
		log.Printf("center_descriptor phase=%s event=done elapsed_ms=%d", phase, time.Since(started).Milliseconds())
	}
}

// ServeHTTP provides bounded read-only endpoints; mount at /api/integration/.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mu.Lock()
	d := h.desc
	h.mu.Unlock()
	var value any
	limit := MaxResponse
	switch r.URL.Path {
	case RuntimeCapabilitiesPath:
		if h.cfg.RuntimeCapabilities == nil {
			http.Error(w, "runtime_capabilities_unavailable", http.StatusServiceUnavailable)
			return
		}
		v, err := h.cfg.RuntimeCapabilities()
		if v.Schema == 0 {
			v.Schema = 1
		}
		if v.InstanceID == "" {
			v.InstanceID = d.InstanceID
		}
		if v.Build == "" {
			v.Build = h.cfg.Build
		}
		if err != nil || validateRuntimeCapabilities(v, d.InstanceID, h.cfg.Build) != nil {
			http.Error(w, "runtime_capabilities_unavailable", http.StatusServiceUnavailable)
			return
		}
		value = v
	case ReportPath:
		if h.cfg.Report == nil {
			http.Error(w, "report_unavailable", 503)
			return
		}
		v, err := h.cfg.Report()
		v.Schema, v.Build, v.InstanceID = 1, h.cfg.Build, d.InstanceID
		if err != nil || validateReport(v) != nil {
			http.Error(w, "report_unavailable", 503)
			return
		}
		value, limit = v, MaxDashboardResponse
	case DashboardPath:
		if h.cfg.Dashboard == nil {
			http.Error(w, "dashboard_unavailable", 503)
			return
		}
		v, err := h.cfg.Dashboard()
		v.Schema, v.Build, v.InstanceID = 1, h.cfg.Build, d.InstanceID
		if err != nil || validateDashboard(v) != nil {
			http.Error(w, "dashboard_unavailable", 503)
			return
		}
		value, limit = v, MaxDashboardResponse
	case FilesPath:
		if h.cfg.ModelFiles == nil {
			http.Error(w, "model_files_unavailable", 503)
			return
		}
		inventory, model, cursor, _ := ParseFilesQuery(r.URL)
		v, err := h.cfg.ModelFiles(inventory, model, cursor)
		v.Schema, v.Build, v.InstanceID = 1, h.cfg.Build, d.InstanceID
		if err != nil || validateFilesPage(v) != nil {
			http.Error(w, "model_files_unavailable", 503)
			return
		}
		value, limit = v, MaxDashboardResponse
	case HealthPath:
		v := Health{State: "ready"}
		if h.cfg.Health != nil {
			v = h.cfg.Health()
		}
		v.Schema = 1
		v.Product = Product
		v.Protocol = Protocol
		v.Build = h.cfg.Build
		v.InstanceID = d.InstanceID
		value = v
	case DependenciesPath:
		v := DependencySnapshot{}
		if h.cfg.Dependencies != nil {
			v = h.cfg.Dependencies()
		}
		v.Schema = 1
		v.Build = h.cfg.Build
		v.InstanceID = d.InstanceID
		if v.ObservedAt.IsZero() {
			v.ObservedAt = h.now()
		}
		if e := validateSnapshot(&v, h.now()); e != nil {
			http.Error(w, "dependencies_invalid", 503)
			return
		}
		value = v
	default:
		http.NotFound(w, r)
		return
	}
	b, e := json.Marshal(value)
	if e != nil || len(b) > limit {
		http.Error(w, "integration_response_invalid", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// validateRuntimeCapabilities rejects partial or mismatched evidence rather than
// stamping it with the current host identity and concealing a stale callback.
func validateRuntimeCapabilities(v RuntimeCapabilities, instanceID, build string) error {
	if v.Schema != 1 || v.InstanceID == "" || v.InstanceID != instanceID || v.Build == "" || v.Build != build || v.ObservedAt.IsZero() || len(v.Capabilities) != len(RuntimeScenes()) {
		return errors.New("center_capabilities_invalid")
	}
	seen := make(map[string]bool, len(v.Capabilities))
	for _, capability := range v.Capabilities {
		if capability.Validate() != nil || seen[capability.Scene] {
			return errors.New("center_capabilities_invalid")
		}
		seen[capability.Scene] = true
	}
	return nil
}

func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	l, d := h.lock, h.desc
	h.mu.Unlock()
	if l != nil {
		var current Descriptor
		f, e := os.Open(filepath.Join(h.cfg.Config.DataDir, DescriptorName))
		if e == nil {
			e = decode(f, MaxDescriptor, &current)
			f.Close()
			if e == nil && current.InstanceID == d.InstanceID {
				_ = os.Remove(filepath.Join(h.cfg.Config.DataDir, DescriptorName))
			}
		}
		return l.Close()
	}
	return nil
}
