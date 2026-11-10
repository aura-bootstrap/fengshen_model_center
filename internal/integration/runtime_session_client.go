package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// RuntimeSessionHandle is an in-memory task scope. It carries no secret: session
// and task IDs are routing identifiers used to keep assets and jobs apart.
type RuntimeSessionHandle struct {
	mu    sync.Mutex
	grant SessionScope
}

func (s *RuntimeSessionHandle) Scope() SessionScope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyScope(s.grant)
}
func (s *RuntimeSessionHandle) String() string   { return "[model-center runtime session]" }
func (s *RuntimeSessionHandle) GoString() string { return s.String() }
func (s *RuntimeSessionHandle) Format(state fmt.State, verb rune) {
	_, _ = state.Write([]byte(s.String()))
}

// OpenRuntimeSession creates a local, non-secret task scope. It never sends a
// credential and only ensures the local center process is reachable.
func (c *Client) OpenRuntimeSession(ctx context.Context, task string, capabilities []string, bindings []ExecutionBindingRef) (*RuntimeSessionHandle, error) {
	if !opaqueRuntimeID(task) || len(bindings) == 0 {
		return nil, errors.New("center_session_scope_invalid")
	}
	seen := map[string]bool{}
	for _, scene := range capabilities {
		if _, ok := RuntimeScene(scene); !ok || seen[scene] {
			return nil, errors.New("center_session_scope_invalid")
		}
		seen[scene] = true
	}
	for _, ref := range bindings {
		if ref.Validate() != nil || !seen[ref.Scene] {
			return nil, errors.New("center_session_scope_invalid")
		}
	}
	d, err := c.EnsureRunning(ctx)
	if err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &RuntimeSessionHandle{grant: SessionScope{SessionID: id, InstanceID: d.InstanceID, TaskID: task, Bindings: append([]ExecutionBindingRef(nil), bindings...)}}, nil
}

// SetSessionHeaders adds non-secret session routing headers.
func (c *Client) SetSessionHeaders(s *RuntimeSessionHandle, r *http.Request) error {
	if s == nil || r == nil {
		return errors.New("center_session_invalid")
	}
	g := s.Scope()
	if g.SessionID == "" || g.TaskID == "" {
		return errors.New("center_session_invalid")
	}
	r.Header.Set(RuntimeSessionHeader, g.SessionID)
	r.Header.Set(RuntimeTaskHeader, g.TaskID)
	return nil
}

// HeartbeatRuntimeSession and CloseRuntimeSession are retained as no-op
// lifecycle calls so existing callers keep compiling without credentials.
func (c *Client) HeartbeatRuntimeSession(ctx context.Context, s *RuntimeSessionHandle) error {
	if s == nil {
		return errors.New("center_session_invalid")
	}
	return nil
}
func (c *Client) CloseRuntimeSession(ctx context.Context, s *RuntimeSessionHandle) error {
	if s == nil {
		return errors.New("center_session_invalid")
	}
	return nil
}

func SessionHTTPClient(timeout time.Duration) *http.Client { return newHTTPClient(timeout) }
