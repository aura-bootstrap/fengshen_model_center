package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SessionCredentials is an in-memory engine capability. It intentionally has no
// exported fields, JSON token, string formatter, URL token, or CLI representation.
// Do not hand it to Flutter or persist it in task snapshots.
type SessionCredentials struct {
	// lifecycle serializes heartbeat/rotation/close before taking c.mu then mu.
	lifecycle  sync.Mutex
	mu         sync.Mutex
	token      string
	descriptor Descriptor
	center     processIdentity
	grant      SessionGrant
	started    time.Time
}

func (s *SessionCredentials) Grant() SessionGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyGrant(s.grant)
}
func (s *SessionCredentials) String() string   { return "[model-center session credentials]" }
func (s *SessionCredentials) GoString() string { return s.String() }
func (s *SessionCredentials) Format(state fmt.State, verb rune) {
	_, _ = state.Write([]byte(s.String()))
}

// OpenRuntimeSession is ONLY for an explicitly submitted/resumed task after the
// engine's existing business/device authorization. It may start/confirm the
// center and hash delivery, never a model. Inspect/report/deps do not call it.
func (c *Client) OpenRuntimeSession(ctx context.Context, task string, capabilities []string, bindings []ExecutionBindingRef) (*SessionCredentials, error) {
	return c.openSession(ctx, SessionRequest{Permission: PermissionRuntime, TaskID: task, Capabilities: capabilities, Bindings: bindings})
}

// OpenManagementSession is an explicit trusted engine proxy action. It cannot
// authorize runtime requests; the host independently verifies admin build trust.
func (c *Client) OpenManagementSession(ctx context.Context) (*SessionCredentials, error) {
	return c.openSession(ctx, SessionRequest{Permission: PermissionManagement})
}
func (c *Client) openSession(ctx context.Context, request SessionRequest) (*SessionCredentials, error) {
	if e := request.validate(); e != nil {
		return nil, e
	}
	if _, e := c.EnsureRunning(ctx); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshakeSession(ctx, request, nil)
}

// Caller holds c.mu, and also s.mu for a rotation. No startup or recovery here.
func (c *Client) handshakeSession(ctx context.Context, request SessionRequest, s *SessionCredentials) (*SessionCredentials, error) {
	h, e := c.connect(ctx, s != nil)
	if e != nil {
		return nil, e
	}
	d := h.Descriptor
	if s != nil && (d != s.descriptor || c.cache.Identity != s.center) {
		return nil, ErrUnconfirmed
	}
	conn, pid, e := dialPipe(ctx, d.PipeName)
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	p, e := queryProcess(pid)
	if e != nil || pid != d.PID || p.Created != d.ProcessStartedAt || !c.cached(p) {
		return nil, ErrUnconfirmed
	}
	req := sessionHandshakeRequest{Schema: 1, Protocol: RuntimeProtocol, InstanceID: d.InstanceID, RootKind: d.RootKind, RootIdentity: d.RootIdentity, DataIdentity: d.DataIdentity, Request: request}
	if s != nil {
		req.Action, req.PreviousToken = "rotate", s.token
	}
	started := time.Now()
	if e = writeFrame(conn, req); e != nil {
		return nil, e
	}
	var raw jsonRawSession
	if e = readFrame(conn, &raw); e != nil {
		return nil, e
	}
	var out sessionHandshakeResponse
	if e = strictSessionJSON(raw, &out); e != nil {
		return nil, e
	}
	next, e := queryProcess(pid)
	g := out.Grant
	if e != nil || next != p || out.Schema != 1 || out.Protocol != RuntimeProtocol || out.Descriptor != d || out.Build != h.Build || !digestValid(out.Token) || !digestValid(g.SessionID) || g.InstanceID != d.InstanceID || g.Permission != request.Permission || g.TaskID != request.TaskID || !sameSessionScope(g, request) || g.ExpiresAt.After(g.MaximumAt) || g.ExpiresAt.Before(started) || g.ExpiresAt.After(time.Now().Add(sessionLease)) || g.MaximumAt.After(time.Now().Add(sessionMaximum)) {
		return nil, errors.New("center_session_response_invalid")
	}
	if g.MaximumAt.Before(started) || s != nil && (g.SessionID != s.grant.SessionID || g.Edition != s.grant.Edition || out.Token == s.token) {
		return nil, errors.New("center_session_response_invalid")
	}
	return &SessionCredentials{token: out.Token, descriptor: d, center: p, grant: copyGrant(g), started: started}, nil
}

// Alias retains RawMessage's unmarshalling behavior without exposing a wire DTO.
type jsonRawSession = json.RawMessage

func sameSessionScope(g SessionGrant, r SessionRequest) bool {
	if len(g.Capabilities) != len(r.Capabilities) || len(g.Bindings) != len(r.Bindings) {
		return false
	}
	for i, v := range r.Capabilities {
		if g.Capabilities[i] != v {
			return false
		}
	}
	for i, v := range r.Bindings {
		if g.Bindings[i] != v {
			return false
		}
	}
	return g.Edition == "user" || g.Edition == "admin" || g.Edition == "developer"
}

// AuthenticateSessionRequest adds the appropriate secret header only after live
// center identity and literal-loopback checks. It does not send or retry writes.
// Use SessionHTTPClient (no proxy or redirects) to send the authenticated request.
func (c *Client) AuthenticateSessionRequest(s *SessionCredentials, r *http.Request) error {
	if s == nil || r == nil {
		return errors.New("center_session_invalid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := c.validateSessionLocked(s, time.Now()); e != nil {
		return e
	}
	if r.URL.Scheme != "http" || r.URL.Host != s.descriptor.LoopbackAddress || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Opaque != "" || !sessionBoundary(r, s.descriptor.LoopbackAddress) {
		return errors.New("center_session_request_invalid")
	}
	lifecycle := r.URL.Path == SessionHeartbeatPath && r.Method == "POST" || r.URL.Path == RuntimeSessionPath && r.Method == "DELETE"
	if !lifecycle && !RuntimeRouteAllowed(s.grant.Permission, r.Method, r.URL.Path) {
		return errors.New("center_session_request_invalid")
	}
	for _, header := range []string{RuntimeHeader, ManagementHeader, ReadOnlyHeader, "Authorization", "Cookie"} {
		if len(r.Header.Values(header)) != 0 {
			return errors.New("center_session_request_invalid")
		}
	}
	r.Header.Set(permissionHeader(s.grant.Permission), s.token)
	return nil
}
func (c *Client) validateSessionLocked(s *SessionCredentials, now time.Time) error {
	p, e := queryProcess(s.center.PID)
	if e != nil || p != s.center || !c.cached(p) || s.token == "" || now.Before(s.started) || now.Sub(s.started) >= sessionMaximum || !now.Before(s.grant.ExpiresAt) || !now.Before(s.grant.MaximumAt) {
		s.token = ""
		return errors.New("center_session_expired")
	}
	return nil
}

// Two one-minute heartbeat periods remain before the generation maximum.
const sessionRenewBefore = 2 * time.Minute

func sessionRenewDue(s *SessionCredentials, now time.Time) bool {
	return s.grant.Permission == PermissionRuntime && !now.Before(s.grant.MaximumAt.Add(-sessionRenewBefore))
}

func (c *Client) renewSession(ctx context.Context, s *SessionCredentials) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err != nil {
			s.token = ""
		}
	}()
	if err = c.validateSessionLocked(s, time.Now()); err != nil {
		return err
	}
	request := SessionRequest{Permission: s.grant.Permission, TaskID: s.grant.TaskID, Capabilities: append([]string(nil), s.grant.Capabilities...), Bindings: append([]ExecutionBindingRef(nil), s.grant.Bindings...)}
	if request.Permission != PermissionRuntime {
		return errors.New("center_session_scope_invalid")
	}
	next, err := c.handshakeSession(ctx, request, s)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(next.grant.ExpiresAt) || !time.Now().Before(next.grant.MaximumAt) {
		return errors.New("center_session_expired")
	}
	s.token, s.grant, s.started = next.token, next.grant, next.started
	return nil
}

func SessionHTTPClient(timeout time.Duration) *http.Client { return newHTTPClient(timeout) }
func (c *Client) sessionLifecycle(ctx context.Context, s *SessionCredentials, deleteSession bool) error {
	if s == nil {
		return errors.New("center_session_invalid")
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	due := !deleteSession && sessionRenewDue(s, time.Now())
	address := s.descriptor.LoopbackAddress
	s.mu.Unlock()
	if due {
		return c.renewSession(ctx, s)
	}
	method, path := "POST", SessionHeartbeatPath
	if deleteSession {
		method, path = "DELETE", RuntimeSessionPath
	}
	r, e := http.NewRequestWithContext(ctx, method, "http://"+address+path, nil)
	if e != nil {
		return e
	}
	began := time.Now()
	if e = c.AuthenticateSessionRequest(s, r); e != nil {
		return e
	}
	client := newHTTPClient(3 * time.Second)
	defer client.CloseIdleConnections()
	response, e := client.Do(r)
	if e == nil {
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			e = errors.New("center_session_denied")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e != nil || deleteSession {
		s.token = ""
		return e
	}
	s.grant.ExpiresAt = began.Add(sessionLease)
	if s.grant.ExpiresAt.After(s.grant.MaximumAt) {
		s.grant.ExpiresAt = s.grant.MaximumAt
	}
	return nil
}
func (c *Client) HeartbeatRuntimeSession(ctx context.Context, s *SessionCredentials) error {
	return c.sessionLifecycle(ctx, s, false)
}
func (c *Client) CloseRuntimeSession(ctx context.Context, s *SessionCredentials) error {
	return c.sessionLifecycle(ctx, s, true)
}
