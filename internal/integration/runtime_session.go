package integration

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const SessionHeartbeatPath = RuntimeSessionPath + "/heartbeat"
const sessionLease = 5 * time.Minute
const sessionMaximum = 2 * time.Hour

// TrustedEngine is trusted host configuration, loaded after engine delivery is
// built. Never populate this from a handshake, browser, or discovered process.
// Management must be explicitly enabled for an admin/developer engine.
type TrustedEngine struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Edition    string `json:"edition"`
	Management bool   `json:"management"`
}

type SessionRequest struct {
	Permission   SessionPermission     `json:"permission"`
	TaskID       string                `json:"task_id"`
	Capabilities []string              `json:"capabilities"`
	Bindings     []ExecutionBindingRef `json:"bindings"`
}

func (r SessionRequest) MarshalJSON() ([]byte, error) {
	type wire SessionRequest
	if r.Capabilities == nil {
		r.Capabilities = []string{}
	}
	if r.Bindings == nil {
		r.Bindings = []ExecutionBindingRef{}
	}
	return json.Marshal(wire(r))
}
func (r SessionRequest) validate() error {
	if r.Permission == PermissionManagement {
		if r.TaskID != "" || len(r.Capabilities) != 0 || len(r.Bindings) != 0 {
			return errors.New("center_session_scope_invalid")
		}
		return nil
	}
	if r.Permission != PermissionRuntime || !opaqueRuntimeID(r.TaskID) || len(r.Capabilities) == 0 || len(r.Capabilities) > len(RuntimeScenes()) || len(r.Bindings) != len(r.Capabilities) {
		return errors.New("center_session_scope_invalid")
	}
	seen := map[string]bool{}
	for _, scene := range r.Capabilities {
		if _, ok := RuntimeScene(scene); !ok || seen[scene] {
			return errors.New("center_session_scope_invalid")
		}
		seen[scene] = true
	}
	for _, ref := range r.Bindings {
		if ref.Validate() != nil || !seen[ref.Scene] {
			return errors.New("center_session_scope_invalid")
		}
		delete(seen, ref.Scene)
	}
	return nil
}

// SessionGrant contains no credentials. Downstream jobs/assets MUST check Allows
// against their task and frozen binding, and retain SessionID for cancellation.
type SessionGrant struct {
	SessionID    string                `json:"session_id"`
	InstanceID   string                `json:"instance_id"`
	Permission   SessionPermission     `json:"permission"`
	Edition      string                `json:"edition"`
	TaskID       string                `json:"task_id"`
	Capabilities []string              `json:"capabilities"`
	Bindings     []ExecutionBindingRef `json:"bindings"`
	ExpiresAt    time.Time             `json:"expires_at"`
	MaximumAt    time.Time             `json:"maximum_at"`
}

func (g SessionGrant) MarshalJSON() ([]byte, error) {
	type wire SessionGrant
	if g.Capabilities == nil {
		g.Capabilities = []string{}
	}
	if g.Bindings == nil {
		g.Bindings = []ExecutionBindingRef{}
	}
	return json.Marshal(wire(g))
}
func (g SessionGrant) Allows(task string, ref ExecutionBindingRef) bool {
	if g.Permission != PermissionRuntime || g.TaskID != task || ref.Validate() != nil {
		return false
	}
	for _, scene := range g.Capabilities {
		if scene == ref.Scene {
			for _, b := range g.Bindings {
				if b == ref {
					return true
				}
			}
		}
	}
	return false
}
func copyGrant(g SessionGrant) SessionGrant {
	g.Capabilities = append([]string(nil), g.Capabilities...)
	g.Bindings = append([]ExecutionBindingRef(nil), g.Bindings...)
	return g
}

type runtimeSession struct {
	grant SessionGrant
	peer  processIdentity
}
type sessionHandshakeRequest struct {
	Schema        int            `json:"schema"`
	Protocol      string         `json:"protocol"`
	InstanceID    string         `json:"instance_id"`
	RootKind      string         `json:"root_kind"`
	RootIdentity  string         `json:"root_identity"`
	DataIdentity  string         `json:"data_identity"`
	Request       SessionRequest `json:"request"`
	Action        string         `json:"action,omitempty"`
	PreviousToken string         `json:"previous_token,omitempty"`
}
type sessionHandshakeResponse struct {
	Schema     int          `json:"schema"`
	Protocol   string       `json:"protocol"`
	Build      string       `json:"build"`
	Descriptor Descriptor   `json:"descriptor"`
	Grant      SessionGrant `json:"grant"`
	Token      string       `json:"token"`
}

// strictSessionJSON rejects duplicate names (including escaped spellings), null
// at any depth, unknown fields and trailing values. The legacy wire is unchanged.
func strictSessionJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		if t == nil {
			return errors.New("ipc_null_denied")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return errors.New("ipc_duplicate_field")
				}
				seen[s] = true
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		default:
			return errors.New("ipc_json_invalid")
		}
		_, e = d.Token()
		return e
	}
	if e := walk(); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("ipc_trailing_json")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("ipc_object_required")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func (h *Host) processIdentity(pid uint32) (processIdentity, error) {
	if h.process != nil {
		return h.process(pid)
	}
	return queryProcess(pid)
}
func (h *Host) hashEngine(ctx context.Context, path, digest string) error {
	if h.engineHash != nil {
		return h.engineHash(ctx, path, digest, 1024*1024*1024)
	}
	return hashFile(ctx, path, digest, 1024*1024*1024)
}
func (h *Host) admitSession(ctx context.Context, peer processIdentity, req SessionRequest) (sessionHandshakeResponse, error) {
	return h.admitSessionHandshake(ctx, peer, sessionHandshakeRequest{Request: req})
}

func (h *Host) admitSessionHandshake(ctx context.Context, peer processIdentity, wire sessionHandshakeRequest) (sessionHandshakeResponse, error) {
	var out sessionHandshakeResponse
	denied := errors.New("center_engine_untrusted")
	req := wire.Request
	rotate := wire.Action == "rotate"
	if wire.Action != "" && !rotate || rotate && (!digestValid(wire.PreviousToken) || req.Permission != PermissionRuntime) || !rotate && wire.PreviousToken != "" {
		return out, denied
	}
	if e := req.validate(); e != nil {
		return out, e
	}
	if peer.SID == "" || peer.SID != h.sid || peer.PID == 0 || peer.Created == "" || peer.FileID == "" {
		return out, denied
	}
	var trust *TrustedEngine
	for i := range h.cfg.EngineAllowlist {
		v := &h.cfg.EngineAllowlist[i]
		if strings.EqualFold(filepath.Clean(v.Path), filepath.Clean(peer.Image)) {
			trust = v
			break
		}
	}
	if trust == nil || !digestValid(trust.SHA256) || trust.Edition != "user" && trust.Edition != "admin" && trust.Edition != "developer" {
		return out, denied
	}
	if req.Permission == PermissionManagement && (!trust.Management || trust.Edition == "user") {
		return out, denied
	}
	before, e := h.processIdentity(peer.PID)
	if e != nil || before != peer {
		return out, denied
	}
	if e = h.hashEngine(ctx, trust.Path, trust.SHA256); e != nil {
		return out, denied
	}
	after, e := h.processIdentity(peer.PID)
	if e != nil || after != peer {
		return out, denied
	}
	h.SweepSessions()
	token, e := randomID()
	if e != nil {
		return out, e
	}
	id, e := randomID()
	if e != nil {
		return out, e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return out, errors.New("center_closed")
	}
	now := h.now()
	if rotate {
		old := h.sessions[wire.PreviousToken]
		if old == nil || old.peer != peer || old.grant.InstanceID != h.desc.InstanceID || old.grant.Permission != PermissionRuntime || old.grant.Edition != trust.Edition || old.grant.TaskID != req.TaskID || !sameSessionScope(old.grant, req) || !now.Before(old.grant.ExpiresAt) || !now.Before(old.grant.MaximumAt) {
			return out, denied
		}
		// Only the credential generation changes. Already authorized in-flight
		// work retains this exact owner/scope; stale requests are never replayed.
		g := copyGrant(old.grant)
		g.ExpiresAt, g.MaximumAt = now.Add(sessionLease), now.Add(sessionMaximum)
		delete(h.sessions, wire.PreviousToken)
		h.sessions[token] = &runtimeSession{grant: g, peer: peer}
		return sessionHandshakeResponse{Schema: 1, Protocol: RuntimeProtocol, Build: h.cfg.Build, Descriptor: h.desc, Grant: copyGrant(g), Token: token}, nil
	}
	if len(h.sessions) >= 32 {
		return out, errors.New("center_sessions_full")
	}
	if h.sessions == nil {
		h.sessions = map[string]*runtimeSession{}
	}
	g := SessionGrant{SessionID: id, InstanceID: h.desc.InstanceID, Permission: req.Permission, Edition: trust.Edition, TaskID: req.TaskID, Capabilities: append([]string(nil), req.Capabilities...), Bindings: append([]ExecutionBindingRef(nil), req.Bindings...), ExpiresAt: now.Add(sessionLease), MaximumAt: now.Add(sessionMaximum)}
	h.sessions[token] = &runtimeSession{grant: g, peer: peer}
	return sessionHandshakeResponse{Schema: 1, Protocol: RuntimeProtocol, Build: h.cfg.Build, Descriptor: h.desc, Grant: copyGrant(g), Token: token}, nil
}

// SweepSessions performs metadata-only liveness checks; no hashing or process
// termination. Cancellation callbacks run outside the lock and exactly once.
// ServeIPC runs this every second; tests/embedders may also call it explicitly.
func (h *Host) SweepSessions() {
	var revoked []SessionGrant
	h.mu.Lock()
	now := h.now()
	for token, s := range h.sessions {
		p, e := h.processIdentity(s.peer.PID)
		if h.closed || !now.Before(s.grant.ExpiresAt) || !now.Before(s.grant.MaximumAt) || e != nil || p != s.peer {
			delete(h.sessions, token)
			revoked = append(revoked, copyGrant(s.grant))
		}
	}
	h.mu.Unlock()
	h.cancelGrants(revoked)
}
func (h *Host) cancelGrants(grants []SessionGrant) {
	if h.cfg.CancelSession != nil {
		for _, g := range grants {
			h.cfg.CancelSession(g)
		}
	}
}
func sessionBoundary(r *http.Request, address string) bool {
	if r.Host != address || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return false
	}
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	return len(r.Header.Values("Origin")) <= 1 && len(r.Header.Values("Sec-Fetch-Site")) <= 1 && (origin == "" || origin == "http://"+address) && (site == "" || site == "same-origin" || site == "none")
}
func permissionHeader(p SessionPermission) string {
	if p == PermissionManagement {
		return ManagementHeader
	}
	return RuntimeHeader
}
func (h *Host) authorizeSession(r *http.Request, permission SessionPermission, heartbeat bool) (SessionGrant, error) {
	h.SweepSessions()
	denied := errors.New("center_session_denied")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || permission != PermissionManagement && permission != PermissionRuntime || !sessionBoundary(r, h.desc.LoopbackAddress) {
		return SessionGrant{}, denied
	}
	if heartbeat {
		if r.Method != "POST" || r.URL.Path != SessionHeartbeatPath {
			return SessionGrant{}, denied
		}
	} else if !RuntimeRouteAllowed(permission, r.Method, r.URL.Path) {
		return SessionGrant{}, denied
	}
	header := permissionHeader(permission)
	// No mixed browser/read/runtime/management credentials, even if one is valid.
	for _, other := range []string{ReadOnlyHeader, RuntimeHeader, ManagementHeader, "X-Model-Center-Token", "Authorization", "Cookie"} {
		if other != header && len(r.Header.Values(other)) != 0 {
			return SessionGrant{}, denied
		}
	}
	if len(r.Header.Values(header)) != 1 {
		return SessionGrant{}, denied
	}
	token := r.Header.Get(header)
	if !digestValid(token) {
		return SessionGrant{}, denied
	}
	for t, s := range h.sessions {
		if s.grant.Permission == permission && subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 && h.now().Before(s.grant.ExpiresAt) && h.now().Before(s.grant.MaximumAt) {
			// Sweep above is not a substitute for checking the selected peer at admission.
			p, e := h.processIdentity(s.peer.PID)
			if e != nil || p != s.peer {
				return SessionGrant{}, denied
			}
			if heartbeat {
				end := h.now().Add(sessionLease)
				if end.After(s.grant.MaximumAt) {
					end = s.grant.MaximumAt
				}
				s.grant.ExpiresAt = end
			}
			return copyGrant(s.grant), nil
		}
	}
	return SessionGrant{}, denied
}

// AuthorizeSession authenticates only the exact permission's routes. Runtime
// consumers must additionally enforce grant.Allows and SessionID ownership.
func (h *Host) AuthorizeSession(r *http.Request, permission SessionPermission) (SessionGrant, error) {
	return h.authorizeSession(r, permission, false)
}
func (h *Host) HeartbeatSession(r *http.Request) error {
	p := PermissionRuntime
	if len(r.Header.Values(ManagementHeader)) != 0 {
		p = PermissionManagement
	}
	_, e := h.authorizeSession(r, p, true)
	return e
}
func (h *Host) DeleteSession(r *http.Request) error {
	if r.Method != "DELETE" || r.URL.Path != RuntimeSessionPath {
		return errors.New("center_session_denied")
	}
	p := PermissionRuntime
	if len(r.Header.Values(ManagementHeader)) != 0 {
		p = PermissionManagement
	}
	// Management has no runtime route permission, but owns its lifecycle endpoint.
	var g SessionGrant
	var e error
	if p == PermissionManagement {
		clone := r.Clone(r.Context())
		u := *r.URL
		clone.URL = &u
		clone.Method = "GET"
		clone.URL.Path = BindingsPath
		if r.Method != "DELETE" || r.URL.Path != RuntimeSessionPath {
			return errors.New("center_session_denied")
		}
		g, e = h.AuthorizeSession(clone, p)
	} else {
		g, e = h.AuthorizeSession(r, p)
	}
	if e != nil {
		return e
	}
	h.mu.Lock()
	var revoked []SessionGrant
	for token, s := range h.sessions {
		if s.grant.SessionID == g.SessionID {
			delete(h.sessions, token)
			revoked = append(revoked, copyGrant(s.grant))
		}
	}
	h.mu.Unlock()
	h.cancelGrants(revoked)
	return nil
}

// ServeSessionHTTP owns only authenticated lifecycle routes; mount these exact
// paths alongside downstream runtime/management handlers, not as a catch-all.
func (h *Host) ServeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var e error
	if r.URL.Path == SessionHeartbeatPath {
		e = h.HeartbeatSession(r)
	} else if r.URL.Path == RuntimeSessionPath {
		e = h.DeleteSession(r)
	} else {
		http.NotFound(w, r)
		return
	}
	if e != nil {
		http.Error(w, "center_session_denied", 403)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Host) serveSessionIPC(ctx context.Context, rw io.ReadWriter, raw []byte) {
	var req sessionHandshakeRequest
	if strictSessionJSON(raw, &req) != nil {
		return
	}
	h.mu.Lock()
	d := h.desc
	closed := h.closed
	h.mu.Unlock()
	if closed || req.Schema != 1 || req.Protocol != RuntimeProtocol || req.InstanceID != d.InstanceID || req.RootKind != d.RootKind || req.RootIdentity != d.RootIdentity || req.DataIdentity != d.DataIdentity {
		return
	}
	peer, e := pipePeer(rw)
	if e != nil {
		return
	}
	out, e := h.admitSessionHandshake(ctx, peer, req)
	if e != nil {
		return
	}
	if writeFrame(rw, out) != nil {
		h.mu.Lock()
		s := h.sessions[out.Token]
		delete(h.sessions, out.Token)
		h.mu.Unlock()
		if s != nil {
			h.cancelGrants([]SessionGrant{copyGrant(s.grant)})
		}
	}
}
