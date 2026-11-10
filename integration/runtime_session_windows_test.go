//go:build windows

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWindowsRuntimeClientExplicitLifecycle(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FENGSHEN_SESSION_ENGINE", exe)
	t.Setenv("FENGSHEN_SESSION_ENGINE_SHA256", deliveryDigest(raw))
	c := sessionFixture(t)
	s, err := c.OpenRuntimeSession(context.Background(), "task-1", taskRequest().Capabilities, taskRequest().Bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Grant().Allows("task-1", taskRequest().Bindings[0]) {
		t.Fatal("client scope")
	}
	for _, value := range []string{"", "browser-token"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+RuntimeJobsPath, nil)
		r.Header.Set("X-Model-Center-Token", value)
		if err = c.AuthenticateSessionRequest(s, r); err == nil || len(r.Header.Values(RuntimeHeader)) != 0 {
			t.Fatal("client attached runtime secret to mixed browser request")
		}
	}
	if err = c.HeartbeatRuntimeSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	owner, oldToken := s.Grant().SessionID, s.token
	s.mu.Lock()
	s.grant.MaximumAt = time.Now().Add(sessionRenewBefore)
	s.mu.Unlock()
	if err = c.HeartbeatRuntimeSession(context.Background(), s); err != nil {
		t.Fatal("automatic pipe renewal", err)
	}
	if s.Grant().SessionID != owner || s.token == oldToken || time.Until(s.Grant().MaximumAt) < sessionMaximum-time.Minute {
		t.Fatal("credentials not rotated in place")
	}
	if err = c.CloseRuntimeSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err = c.HeartbeatRuntimeSession(context.Background(), s); err == nil {
		t.Fatal("closed credential reused")
	}
	m, err := c.OpenManagementSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.HeartbeatRuntimeSession(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseRuntimeSession(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Inspect(context.Background()); err != nil {
		t.Fatal("session close stopped center", err)
	}
}

func TestWindowsRuntimeConcurrentClientLifecycle(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FENGSHEN_SESSION_ENGINE", exe)
	t.Setenv("FENGSHEN_SESSION_ENGINE_SHA256", deliveryDigest(raw))
	c := sessionFixture(t)
	for i := 0; i < 5; i++ {
		s, err := c.OpenRuntimeSession(context.Background(), "task-1", taskRequest().Capabilities, taskRequest().Bindings)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.grant.MaximumAt = time.Now().Add(sessionRenewBefore)
		s.mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = c.HeartbeatRuntimeSession(context.Background(), s) }()
		go func() { defer wg.Done(); _ = c.CloseRuntimeSession(context.Background(), s) }()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "http://"+s.descriptor.LoopbackAddress+RuntimeJobsPath, nil)
			_ = c.AuthenticateSessionRequest(s, r)
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("client lock order deadlock")
		}
		s.mu.Lock()
		token := s.token
		s.mu.Unlock()
		if token != "" {
			t.Fatal("close resurrected credential")
		}
	}
}

func TestWindowsRuntimeRenewalFailureClosed(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FENGSHEN_SESSION_ENGINE", exe)
	t.Setenv("FENGSHEN_SESSION_ENGINE_SHA256", deliveryDigest(raw))
	c := sessionFixture(t)
	for _, mode := range []string{"expired", "maximum", "started", "scope", "center", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, err := c.OpenRuntimeSession(context.Background(), "task-1", taskRequest().Capabilities, taskRequest().Bindings)
			if err != nil {
				t.Fatal(err)
			}
			s.grant.MaximumAt = time.Now().Add(sessionRenewBefore)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "expired":
				s.grant.ExpiresAt = time.Now()
			case "maximum":
				s.grant.MaximumAt = time.Now()
			case "started":
				s.started = time.Now().Add(-sessionMaximum)
			case "scope":
				s.grant.Bindings[0].Revision++
			case "center":
				s.descriptor.InstanceID = strings.Repeat("f", 64)
			case "cancelled":
				cancel()
			}
			if err = c.HeartbeatRuntimeSession(ctx, s); err == nil || s.token != "" {
				t.Fatal("failed renewal retained credential")
			}
			if err = c.HeartbeatRuntimeSession(context.Background(), s); err == nil || s.token != "" {
				t.Fatal("failed renewal blindly recovered")
			}
		})
	}
}

func TestWindowsRuntimeActualPipePeer(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(raw)
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "private")
	h, e := NewHost(HostConfig{Config: Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data}, Address: "127.0.0.1:18120", Build: "runtime-test", EngineAllowlist: []TrustedEngine{{Path: exe, SHA256: hex.EncodeToString(digest[:]), Edition: "admin", Management: true}}})
	if e != nil {
		t.Fatal(e)
	}
	if e = h.Register(); e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeIPC(ctx) }()
	defer func() { cancel(); <-done }()
	exchange := func(v any, out any) error {
		conn, _, e := dialPipe(ctx, h.desc.PipeName)
		if e != nil {
			return e
		}
		defer conn.Close()
		if e = writeFrame(conn, v); e != nil {
			return e
		}
		return readFrame(conn, out)
	}
	req := sessionHandshakeRequest{Schema: 1, Protocol: RuntimeProtocol, InstanceID: h.desc.InstanceID, RootKind: h.desc.RootKind, RootIdentity: h.desc.RootIdentity, DataIdentity: h.desc.DataIdentity, Request: taskRequest()}
	var out sessionHandshakeResponse
	if e = exchange(req, &out); e != nil {
		t.Fatal(e)
	}
	h.mu.Lock()
	peer := h.sessions[out.Token].peer
	h.mu.Unlock()
	actual, e := queryProcess(uint32(os.Getpid()))
	if e != nil || peer != actual || peer.SID != h.sid {
		t.Fatalf("actual peer mismatch: %v", e)
	}
	// Legacy handshake remains byte-compatible after runtime dispatch.
	var old handshakeResponse
	if e = exchange(handshakeRequest{Schema: 1, Protocol: Protocol, InstanceID: h.desc.InstanceID, RootKind: h.desc.RootKind, RootIdentity: h.desc.RootIdentity, DataIdentity: h.desc.DataIdentity}, &old); e != nil || old.Token == "" {
		t.Fatal("legacy handshake", e)
	}
	// Self-reported PID/edition are not part of the independent protocol.
	b, _ := json.Marshal(req)
	b = append(b[:len(b)-1], []byte(`,"pid":1,"edition":"admin"}`)...)
	var rejected sessionHandshakeResponse
	if e = exchange(json.RawMessage(b), &rejected); e == nil {
		t.Fatal("claimed identity accepted")
	}
	req.Request = SessionRequest{Permission: PermissionManagement}
	if e = exchange(req, &out); e != nil || out.Grant.Edition != "admin" {
		t.Fatal("management", e)
	}
	h.mu.Lock()
	h.cfg.EngineAllowlist = nil
	h.mu.Unlock()
	if e = exchange(req, &rejected); e == nil {
		t.Fatal("unknown engine accepted")
	}
}

// This helper stays alive until stdin closes, and is never force-killed.
func TestWindowsRuntimeExitHelper(t *testing.T) {
	if os.Getenv("FENGSHEN_RUNTIME_EXIT_HELPER") != "1" {
		return
	}
	os.Stdout.Write([]byte("ready\n"))
	io.Copy(io.Discard, os.Stdin)
}
func TestWindowsRuntimeRealProcessExitCancels(t *testing.T) {
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestWindowsRuntimeExitHelper$")
	cmd.Env = append(os.Environ(), "FENGSHEN_RUNTIME_EXIT_HELPER=1")
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	exited := false
	defer func() {
		stdin.Close()
		if !exited {
			cmd.Wait()
		}
	}()
	ready := make([]byte, 6)
	if _, e = io.ReadFull(stdout, ready); e != nil {
		t.Fatal(e)
	}
	peer, e := queryProcess(uint32(cmd.Process.Pid))
	if e != nil {
		t.Fatal(e)
	}
	h, _, _, cancelled := runtimeFixture(t)
	h.sid = peer.SID
	h.process = nil
	h.cfg.EngineAllowlist = []TrustedEngine{{Path: exe, SHA256: strings.Repeat("a", 64), Edition: "user"}}
	if _, e = h.admitSession(context.Background(), peer, taskRequest()); e != nil {
		t.Fatal(e)
	}
	stdin.Close()
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	exited = true
	h.SweepSessions()
	if *cancelled != 1 {
		t.Fatal("exited peer retained jobs")
	}
	h.Close()
}
