package integration

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

// HostConfig is supplied after the real HTTP listener and registry lock exist.
// Build is a stable build identifier, NOT the self-referential EXE digest.
// Read callbacks must return existing evidence, never scan or launch processes.
type HostConfig struct {
	Config       Config
	Address      string
	Build        string
	Health       func() Health
	Dependencies func() DependencySnapshot
	Dashboard    func() (Dashboard, error)
	Report       func() (Report, error)
	ModelFiles   func(inventoryID, modelID, cursor string) (FilesPage, error)
	// RuntimeCapabilities returns existing evidence only; never scan, hash, or launch.
	RuntimeCapabilities func() (RuntimeCapabilities, error)
	EngineAllowlist     []TrustedEngine
	// CancelSession cancels only work owned by SessionID, never the center or
	// external processes. Called once, outside host locks; must return promptly.
	CancelSession func(SessionGrant)
}
type Host struct {
	mu         sync.Mutex
	cfg        HostConfig
	desc       Descriptor
	tokens     map[string]time.Time
	now        func() time.Time
	lock       io.Closer
	pipe       io.Closer
	closed     bool
	sessions   map[string]*runtimeSession
	sid        string
	process    func(uint32) (processIdentity, error)
	engineHash func(context.Context, string, string, int64) error
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
	sid, e := currentSID()
	if e != nil {
		return nil, e
	}
	cfg.EngineAllowlist = append([]TrustedEngine(nil), cfg.EngineAllowlist...)
	seen := map[string]bool{}
	for _, engine := range cfg.EngineAllowlist {
		key := strings.ToLower(filepath.Clean(engine.Path))
		if !filepath.IsAbs(engine.Path) || !digestValid(engine.SHA256) || seen[key] || (engine.Edition != "user" && engine.Edition != "admin" && engine.Edition != "developer") || (engine.Management && engine.Edition == "user") {
			return nil, errors.New("center_engine_trust_invalid")
		}
		if e := platformCheckPath(engine.Path); e != nil {
			return nil, errors.New("center_engine_trust_invalid")
		}
		seen[key] = true
	}
	return &Host{cfg: cfg, now: time.Now, tokens: map[string]time.Time{}, sessions: map[string]*runtimeSession{}, sid: sid}, nil
}

// Register acquires a private instance lock and publishes an atomic, token-free
// descriptor only after creating the SID-restricted named pipe.
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
	name, e := pipeName(h.cfg.Config.DataDir)
	if e != nil {
		return e
	}
	pipe, e := listenPipe(name)
	if e != nil {
		return e
	}
	h.desc = Descriptor{Schema: 1, InstanceID: id, PID: p.PID, ProcessStartedAt: p.Created, LoopbackAddress: h.cfg.Address, PipeName: name, RootKind: h.cfg.Config.RootKind, RootIdentity: root, DataIdentity: data}
	b, e := json.Marshal(h.desc)
	if e != nil {
		pipe.Close()
		return e
	}
	f, e := os.CreateTemp(h.cfg.Config.DataDir, ".descriptor-*")
	if e != nil {
		pipe.Close()
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
		pipe.Close()
		return e
	}
	h.pipe = pipe
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

// ServeIPC serves one bounded request per connection until ctx cancellation.
// Call Register first. Cancel and wait for ServeIPC before disposing the host.
func (h *Host) ServeIPC(ctx context.Context) error {
	h.mu.Lock()
	p := h.pipe
	h.mu.Unlock()
	if p == nil {
		return errors.New("center_not_registered")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				h.SweepSessions()
			}
		}
	}()
	return servePipe(ctx, p, func(rw io.ReadWriter) {
		var raw json.RawMessage
		if readFrame(rw, &raw) != nil {
			return
		}
		var discriminator struct {
			Protocol string `json:"protocol"`
		}
		if json.Unmarshal(raw, &discriminator) != nil {
			return
		}
		if discriminator.Protocol == RuntimeProtocol {
			h.serveSessionIPC(ctx, rw, raw)
			return
		}
		var req handshakeRequest
		if decode(bytes.NewReader(raw), MaxDescriptor, &req) != nil {
			return
		}
		h.mu.Lock()
		d := h.desc
		closed := h.closed
		h.mu.Unlock()
		if closed || req.Schema != 1 || req.Protocol != Protocol || req.InstanceID != d.InstanceID || req.RootKind != d.RootKind || req.RootIdentity != d.RootIdentity || req.DataIdentity != d.DataIdentity {
			return
		}
		token, e := h.issue()
		if e != nil {
			return
		}
		_ = writeFrame(rw, handshakeResponse{Schema: 1, Protocol: Protocol, Build: h.cfg.Build, Descriptor: d, Token: token})
	})
}
func (h *Host) issue() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", errors.New("center_closed")
	}
	now := h.now()
	for t, end := range h.tokens {
		if !now.Before(end) {
			delete(h.tokens, t)
		}
	}
	if len(h.tokens) >= 32 {
		return "", errors.New("center_sessions_full")
	}
	t, e := randomID()
	if e != nil {
		return "", e
	}
	h.tokens[t] = now.Add(60 * time.Second)
	return t, nil
}

// AuthorizeReadOnly is intentionally limited to exact read-only GET routes. The
// application's outer Host/Origin protections should remain enabled as well.
func (h *Host) AuthorizeReadOnly(r *http.Request) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || r.Method != "GET" || r.Host != h.desc.LoopbackAddress || r.URL.RawPath != "" {
		return false
	}
	switch r.URL.Path {
	case HealthPath, DependenciesPath, DashboardPath, ReportPath, RuntimeCapabilitiesPath:
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			return false
		}
	case FilesPath:
		if _, _, _, e := ParseFilesQuery(r.URL); e != nil {
			return false
		}
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if len(r.Header.Values("Origin")) > 1 || origin != "" && origin != "http://"+h.desc.LoopbackAddress {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if len(r.Header.Values("Sec-Fetch-Site")) > 1 || site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if len(r.Header.Values(RuntimeHeader)) != 0 || len(r.Header.Values(ManagementHeader)) != 0 || len(r.Header.Values("X-Model-Center-Token")) != 0 || len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("Cookie")) != 0 {
		return false
	}
	if len(r.Header.Values(ReadOnlyHeader)) != 1 {
		return false
	}
	v := r.Header.Get(ReadOnlyHeader)
	if len(v) != 64 {
		return false
	}
	valid := false
	now := h.now()
	for t, end := range h.tokens {
		if now.Before(end) && subtle.ConstantTimeCompare([]byte(v), []byte(t)) == 1 {
			valid = true
		}
	}
	return valid
}

// ServeHTTP provides bounded read-only endpoints; mount at /api/integration/.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.AuthorizeReadOnly(r) {
		http.Error(w, "integration_denied", http.StatusForbidden)
		return
	}
	h.mu.Lock()
	d := h.desc
	h.mu.Unlock()
	var value any
	limit := MaxResponse
	if r.URL.Path == RuntimeCapabilitiesPath {
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
	} else if r.URL.Path == ReportPath {
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
	} else if r.URL.Path == DashboardPath {
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
	} else if r.URL.Path == FilesPath {
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
	} else if r.URL.Path == HealthPath {
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
	} else {
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
	clear(h.tokens)
	var revoked []SessionGrant
	for token, s := range h.sessions {
		revoked = append(revoked, copyGrant(s.grant))
		delete(h.sessions, token)
	}
	p, l, d := h.pipe, h.lock, h.desc
	h.mu.Unlock()
	h.cancelGrants(revoked)
	if p != nil {
		p.Close()
	}
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
