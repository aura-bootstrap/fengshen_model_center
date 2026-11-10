package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func runtimeFixture(t *testing.T) (*Host, *processIdentity, *time.Time, *int) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	peer := &processIdentity{PID: 42, Created: "created", Image: "engine.exe", FileID: "file", Size: 10, Modified: 1, SID: "user"}
	cancelled := new(int)
	h := &Host{cfg: HostConfig{EngineAllowlist: []TrustedEngine{{Path: "engine.exe", SHA256: strings.Repeat("a", 64), Edition: "user"}}, CancelSession: func(SessionGrant) { *cancelled++ }}, desc: Descriptor{InstanceID: "instance", LoopbackAddress: "127.0.0.1:18120"}, now: func() time.Time { return now }, tokens: map[string]time.Time{}, sessions: map[string]*runtimeSession{}, process: func(uint32) (processIdentity, error) { return *peer, nil }, engineHash: func(context.Context, string, string, int64) error { return nil }, sid: "user"}
	return h, peer, &now, cancelled
}
func taskRequest() SessionRequest {
	return SessionRequest{Permission: PermissionRuntime, TaskID: "task-1", Capabilities: []string{"highlight.score"}, Bindings: []ExecutionBindingRef{{Scene: "highlight.score", Revision: 1, SHA256: strings.Repeat("b", 64)}}}
}
func sessionHTTP(h *Host, token string, permission SessionPermission) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+RuntimeJobsPath, nil)
	if permission == PermissionManagement {
		r = httptest.NewRequest("GET", "http://127.0.0.1:18120"+BindingsPath, nil)
	}
	r.Header.Set(RuntimeHeader, token)
	if permission == PermissionManagement {
		r.Header.Del(RuntimeHeader)
		r.Header.Set(ManagementHeader, token)
	}
	w := httptest.NewRecorder()
	if _, err := h.AuthorizeSession(r, permission); err != nil {
		w.WriteHeader(403)
	} else {
		w.WriteHeader(200)
	}
	return w
}
func TestRuntimeSessionLeaseIdentityAndScope(t *testing.T) {
	h, p, now, cancelled := runtimeFixture(t)
	s, err := h.admitSession(context.Background(), *p, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if sessionHTTP(h, s.Token, PermissionRuntime).Code != 200 {
		t.Fatal("runtime denied")
	}
	if sessionHTTP(h, s.Token, PermissionManagement).Code != 403 {
		t.Fatal("runtime became management")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+RuntimeJobsPath, nil)
	r.Header.Set(RuntimeHeader, s.Token)
	grant, err := h.AuthorizeSession(r, PermissionRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Allows("task-1", taskRequest().Bindings[0]) || grant.Allows("task-2", taskRequest().Bindings[0]) {
		t.Fatal("task scope lost")
	}
	grant.Bindings[0].Revision++
	grant2, _ := h.AuthorizeSession(r, PermissionRuntime)
	if grant2.Bindings[0].Revision != 1 {
		t.Fatal("mutable grant escaped")
	}
	*now = now.Add(4 * time.Minute)
	heartbeat := httptest.NewRequest("POST", "http://127.0.0.1:18120"+SessionHeartbeatPath, nil)
	heartbeat.Header.Set(RuntimeHeader, s.Token)
	if err = h.HeartbeatSession(heartbeat); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(4 * time.Minute)
	h.SweepSessions()
	if *cancelled != 0 {
		t.Fatal("heartbeat ignored")
	}
	p.Created = "reused"
	h.SweepSessions()
	if *cancelled != 1 || sessionHTTP(h, s.Token, PermissionRuntime).Code != 403 {
		t.Fatal("PID reuse retained session")
	}
	h.Close()
	if *cancelled != 1 {
		t.Fatal("duplicate cancel")
	}
}
func TestRuntimeSessionAdmissionAndLimits(t *testing.T) {
	for _, mode := range []string{"unknown", "sid", "hash", "changed", "management", "task", "scene", "binding"} {
		t.Run(mode, func(t *testing.T) {
			h, p, _, _ := runtimeFixture(t)
			req := taskRequest()
			switch mode {
			case "unknown":
				h.cfg.EngineAllowlist = nil
			case "sid":
				p.SID = "other"
			case "hash":
				h.engineHash = func(context.Context, string, string, int64) error { return errors.New("mismatch") }
			case "changed":
				h.engineHash = func(context.Context, string, string, int64) error { p.Modified++; return nil }
			case "management":
				req = SessionRequest{Permission: PermissionManagement}
			case "task":
				req.TaskID = ""
			case "scene":
				req.Capabilities[0] = "unknown"
			case "binding":
				req.Bindings[0].Scene = "image.face"
			}
			if _, err := h.admitSession(context.Background(), *p, req); err == nil {
				t.Fatal("invalid admission accepted")
			}
		})
	}
	h, p, now, cancelled := runtimeFixture(t)
	for i := 0; i < 32; i++ {
		if _, err := h.admitSession(context.Background(), *p, taskRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.admitSession(context.Background(), *p, taskRequest()); err == nil {
		t.Fatal("session cap ignored")
	}
	*now = now.Add(5 * time.Minute)
	h.SweepSessions()
	if *cancelled != 32 {
		t.Fatal("lease did not cancel", *cancelled)
	}
	if _, err := h.admitSession(context.Background(), *p, taskRequest()); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if *cancelled != 33 {
		t.Fatal("close did not cancel")
	}
}
func TestRuntimeSessionRotationPreservesOwnerAndSlots(t *testing.T) {
	h, p, now, cancelled := runtimeFixture(t)
	first, err := h.admitSession(context.Background(), *p, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 32; i++ {
		if _, err := h.admitSession(context.Background(), *p, taskRequest()); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Minute)
	next, err := h.admitSessionHandshake(context.Background(), *p, sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token})
	if err != nil {
		t.Fatal(err)
	}
	// A job/asset retains this exact owner ID; no migration or TaskID lookup.
	ownerID := first.Grant.SessionID
	if next.Grant.SessionID != ownerID || next.Token == first.Token || next.Grant.MaximumAt != now.Add(sessionMaximum) || next.Grant.ExpiresAt != now.Add(sessionLease) || len(h.sessions) != 32 || *cancelled != 0 {
		t.Fatal("rotation changed owner, cancelled work, or consumed a slot")
	}
	if sessionHTTP(h, first.Token, PermissionRuntime).Code != 403 || sessionHTTP(h, next.Token, PermissionRuntime).Code != 200 {
		t.Fatal("token switch not atomic")
	}
	if _, err := h.admitSessionHandshake(context.Background(), *p, sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token}); err == nil {
		t.Fatal("old proof reused")
	}
}

func TestRuntimeSessionRotationBeforeTwoHourBoundary(t *testing.T) {
	h, p, now, cancelled := runtimeFixture(t)
	hashes := 0
	h.engineHash = func(context.Context, string, string, int64) error { hashes++; return nil }
	first, err := h.admitSession(context.Background(), *p, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewRequest("POST", "http://127.0.0.1:18120"+SessionHeartbeatPath, nil)
	heartbeat.Header.Set(RuntimeHeader, first.Token)
	for i := 0; i < 118; i++ {
		*now = now.Add(time.Minute)
		if err = h.HeartbeatSession(heartbeat); err != nil {
			t.Fatal(err)
		}
	}
	if hashes != 1 {
		t.Fatal("ordinary heartbeat hashed delivery")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+RuntimeJobsPath, nil)
	r.Header.Set(RuntimeHeader, first.Token)
	inFlight, err := h.AuthorizeSession(r, PermissionRuntime)
	if err != nil {
		t.Fatal(err)
	}
	next, err := h.admitSessionHandshake(context.Background(), *p, sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token})
	if err != nil {
		t.Fatal(err)
	}
	*now = first.Grant.MaximumAt
	if sessionHTTP(h, next.Token, PermissionRuntime).Code != 200 || hashes != 2 || *cancelled != 0 || inFlight.SessionID != next.Grant.SessionID || !inFlight.Allows(next.Grant.TaskID, next.Grant.Bindings[0]) {
		t.Fatal("rotation lost in-flight owner/scope or two-hour continuation")
	}
}

func TestRuntimeSessionRotationRejectsChangedProof(t *testing.T) {
	for _, mode := range []string{"task", "binding", "capabilities", "peer", "edition", "permission", "expired", "maximum", "action", "missing", "hash", "closed"} {
		t.Run(mode, func(t *testing.T) {
			h, p, now, _ := runtimeFixture(t)
			first, err := h.admitSession(context.Background(), *p, taskRequest())
			if err != nil {
				t.Fatal(err)
			}
			req := sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token}
			switch mode {
			case "task":
				req.Request.TaskID = "another-task"
			case "binding":
				req.Request.Bindings[0].Revision++
			case "capabilities":
				req.Request.Capabilities[0] = "highlight.transcribe"
				req.Request.Bindings[0].Scene = "highlight.transcribe"
			case "peer":
				p.Created += "other"
			case "edition":
				h.cfg.EngineAllowlist[0].Edition = "admin"
			case "permission":
				req.Request = SessionRequest{Permission: PermissionManagement}
				h.cfg.EngineAllowlist[0].Edition = "admin"
				h.cfg.EngineAllowlist[0].Management = true
			case "expired":
				*now = first.Grant.ExpiresAt
			case "maximum":
				*now = first.Grant.MaximumAt
				h.sessions[first.Token].grant.ExpiresAt = now.Add(time.Minute)
			case "action":
				req.Action = ""
			case "missing":
				req.PreviousToken = ""
			case "hash":
				h.engineHash = func(context.Context, string, string, int64) error { return errors.New("changed") }
			case "closed":
				h.Close()
			}
			if _, err := h.admitSessionHandshake(context.Background(), *p, req); err == nil {
				t.Fatal("changed rotation accepted")
			}
		})
	}
}

func TestRuntimeSessionConcurrentRotationCloseHeartbeat(t *testing.T) {
	for i := 0; i < 30; i++ {
		h, p, _, cancelled := runtimeFixture(t)
		first, err := h.admitSession(context.Background(), *p, taskRequest())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = h.admitSessionHandshake(context.Background(), *p, sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token})
		}()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+SessionHeartbeatPath, nil)
			r.Header.Set(RuntimeHeader, first.Token)
			_ = h.HeartbeatSession(r)
		}()
		go func() { defer wg.Done(); _ = h.Close() }()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("host lifecycle deadlock")
		}
		if len(h.sessions) != 0 || *cancelled != 1 {
			t.Fatal("rotation resurrected closed owner or cancelled twice")
		}
	}
}

func TestRuntimeSessionRenewalTiming(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &SessionCredentials{grant: SessionGrant{Permission: PermissionRuntime, MaximumAt: now.Add(sessionMaximum)}}
	boundary := s.grant.MaximumAt.Add(-2 * time.Minute)
	if sessionRenewDue(s, boundary.Add(-time.Nanosecond)) || !sessionRenewDue(s, boundary) || !sessionRenewDue(s, boundary.Add(time.Minute)) {
		t.Fatal("renewal must start two heartbeat periods before maximum")
	}
	s.grant.Permission = PermissionManagement
	if sessionRenewDue(s, boundary) {
		t.Fatal("management rotation enabled")
	}
}

func TestRuntimeSessionManagementCannotRotateToRuntime(t *testing.T) {
	h, p, _, _ := runtimeFixture(t)
	h.cfg.EngineAllowlist[0].Edition, h.cfg.EngineAllowlist[0].Management = "admin", true
	first, err := h.admitSession(context.Background(), *p, SessionRequest{Permission: PermissionManagement})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.admitSessionHandshake(context.Background(), *p, sessionHandshakeRequest{Request: taskRequest(), Action: "rotate", PreviousToken: first.Token}); err == nil {
		t.Fatal("management became runtime")
	}
}

func TestRuntimeStrictIPCJSON(t *testing.T) {
	for _, raw := range []string{`null`, `{"protocol":"x","protocol":"y"}`, `{"permission":null}`, `{"unknown":1}`, `{"bindings":[{"scene":"x","scene":"y"}]}`} {
		var req sessionHandshakeRequest
		if strictSessionJSON([]byte(raw), &req) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var req sessionHandshakeRequest
	raw, _ := json.Marshal(sessionHandshakeRequest{Schema: 1, Protocol: RuntimeProtocol, Request: taskRequest()})
	if err := strictSessionJSON(raw, &req); err != nil {
		t.Fatal(err)
	}
	// Tokens are not serializable as an application snapshot.
	b, _ := json.Marshal(&SessionCredentials{token: "secret"})
	if bytes.Contains(b, []byte("secret")) {
		t.Fatal("token serialized")
	}
}
