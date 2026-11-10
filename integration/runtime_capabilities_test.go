package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func runtimeCapabilitiesFixture(t *testing.T) (*Host, string, RuntimeCapabilities) {
	t.Helper()
	h, _, _, _ := runtimeFixture(t)
	h.cfg.Build = "test-build"
	token, err := h.issue()
	if err != nil {
		t.Fatal(err)
	}
	v := RuntimeCapabilities{Schema: 1, InstanceID: h.desc.InstanceID, Build: h.cfg.Build, ObservedAt: h.now()}
	for _, scene := range RuntimeScenes() {
		v.Capabilities = append(v.Capabilities, RuntimeCapability{Scene: scene.ID, State: CapabilityMissingBinding})
	}
	return h, token, v
}

func capabilitiesHTTP(h *Host, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://127.0.0.1:18120"+RuntimeCapabilitiesPath, nil)
	r.Header.Set(ReadOnlyHeader, token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRuntimeCapabilitiesReadOnly(t *testing.T) {
	h, token, v := runtimeCapabilitiesFixture(t)
	calls := 0
	h.cfg.RuntimeCapabilities = func() (RuntimeCapabilities, error) { calls++; return v, nil }
	h.process = func(uint32) (processIdentity, error) { t.Fatal("read checked process"); return processIdentity{}, nil }
	h.engineHash = func(context.Context, string, string, int64) error { t.Fatal("read hashed engine"); return nil }
	h.cfg.Health = func() Health { t.Fatal("wrong callback"); return Health{} }
	h.cfg.Dependencies = func() DependencySnapshot { t.Fatal("read scanned dependencies"); return DependencySnapshot{} }
	w := capabilitiesHTTP(h, token)
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	var got RuntimeCapabilities
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("snapshot changed: %+v err=%v", got, err)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing response protections")
	}
	if len(h.sessions) != 0 {
		t.Fatal("read created session")
	}
}

func TestRuntimeCapabilitiesStates(t *testing.T) {
	for _, state := range []CapabilityState{CapabilityMissingBinding, CapabilityMissingComponent, CapabilityIncompatible, CapabilityAvailable, CapabilityBusy, CapabilityFailed} {
		t.Run(string(state), func(t *testing.T) {
			h, token, v := runtimeCapabilitiesFixture(t)
			for i := range v.Capabilities {
				c := &v.Capabilities[i]
				c.State = state
				if state == CapabilityAvailable || state == CapabilityBusy {
					definition, _ := RuntimeScene(c.Scene)
					c.Binding = &ExecutionBindingRef{Scene: c.Scene, Revision: 1, SHA256: strings.Repeat("a", 64)}
					c.Component = &ExecutionComponentRef{ID: definition.Components[0], Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "test/v1"}
				}
			}
			h.cfg.RuntimeCapabilities = func() (RuntimeCapabilities, error) { return v, nil }
			if w := capabilitiesHTTP(h, token); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRuntimeCapabilitiesInvalidResponse(t *testing.T) {
	for _, mode := range []string{"missing-callback", "callback-error", "schema", "instance", "build", "observed", "empty", "missing-scene", "duplicate", "unknown-scene", "invalid-state", "invalid-binding", "invalid-component", "invented-inference", "unsafe-reason", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			h, token, v := runtimeCapabilitiesFixture(t)
			var callbackErr error
			switch mode {
			case "callback-error":
				callbackErr = errors.New("private callback failure")
			case "schema":
				v.Schema++
			case "instance":
				v.InstanceID = "other"
			case "build":
				v.Build = "other"
			case "observed":
				v.ObservedAt = RuntimeCapabilities{}.ObservedAt
			case "empty":
				v.Capabilities = nil
			case "missing-scene":
				v.Capabilities = v.Capabilities[1:]
			case "duplicate":
				v.Capabilities[1] = v.Capabilities[0]
			case "unknown-scene":
				v.Capabilities[0].Scene = "unknown"
			case "invalid-state":
				v.Capabilities[0].State = "ready"
			case "invalid-binding":
				v.Capabilities[0].Binding = &ExecutionBindingRef{Scene: v.Capabilities[0].Scene}
			case "invalid-component":
				v.Capabilities[0].Component = &ExecutionComponentRef{ID: "unknown"}
			case "invented-inference":
				v.Capabilities[0].Inference = &InferenceEvidence{State: "passed"}
			case "unsafe-reason":
				v.Capabilities[0].Reason = `C:\private\models`
			case "oversize":
				v.Build = strings.Repeat("a", MaxResponse)
				h.cfg.Build = v.Build
			}
			if mode != "missing-callback" {
				h.cfg.RuntimeCapabilities = func() (RuntimeCapabilities, error) { return v, callbackErr }
			}
			w := capabilitiesHTTP(h, token)
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRuntimeCapabilityVADProjection(t *testing.T) {
	for _, scene := range RuntimeScenes() {
		for _, enabled := range []bool{false, true} {
			c := RuntimeCapability{Scene: scene.ID, State: CapabilityMissingComponent, VADEnabled: &enabled}
			if got := c.Validate(); (got == nil) != (scene.ID == "highlight.transcribe") {
				t.Fatalf("scene=%s enabled=%v validation=%v", scene.ID, enabled, got)
			}
			if scene.ID != "highlight.transcribe" {
				continue
			}
			raw, err := json.Marshal(c)
			if err != nil || !strings.Contains(string(raw), `"vad_enabled":`) {
				t.Fatalf("explicit projection omitted: %s %v", raw, err)
			}
			var restored RuntimeCapability
			if json.Unmarshal(raw, &restored) != nil || restored.VADEnabled == nil || *restored.VADEnabled != enabled {
				t.Fatal("projection did not round trip")
			}
		}
	}
	c := RuntimeCapability{Scene: "highlight.transcribe", State: CapabilityMissingBinding}
	raw, err := json.Marshal(c)
	if err != nil || c.Validate() != nil || strings.Contains(string(raw), "vad_enabled") {
		t.Fatal("unknown projection must remain absent")
	}
}

func TestRuntimeCapabilitiesExactReadRoute(t *testing.T) {
	for _, mode := range []string{"POST", "PUT", "DELETE", "HEAD", "query", "empty-query", "escaped", "trailing-slash", "missing-token", "runtime-token", "management-token", "mixed-runtime", "mixed-management", "mixed-browser", "empty-browser", "duplicate-token"} {
		t.Run(mode, func(t *testing.T) {
			h, token, _ := runtimeCapabilitiesFixture(t)
			h.cfg.RuntimeCapabilities = func() (RuntimeCapabilities, error) {
				t.Fatal("denied request invoked callback")
				return RuntimeCapabilities{}, nil
			}
			r := httptest.NewRequest("GET", "http://127.0.0.1:18120"+RuntimeCapabilitiesPath, nil)
			r.Header.Set(ReadOnlyHeader, token)
			switch mode {
			case "POST", "PUT", "DELETE", "HEAD":
				r.Method = mode
			case "query":
				r.URL.RawQuery = "token=x"
			case "empty-query":
				r.URL.ForceQuery = true
			case "escaped":
				r.URL.RawPath = "/api/integration/runtime%2dcapabilities"
			case "trailing-slash":
				r.URL.Path += "/"
			case "missing-token":
				r.Header.Del(ReadOnlyHeader)
			case "runtime-token", "management-token":
				r.Header.Del(ReadOnlyHeader)
				header := RuntimeHeader
				if mode == "management-token" {
					header = ManagementHeader
				}
				r.Header.Set(header, token)
			case "mixed-runtime":
				r.Header.Set(RuntimeHeader, "")
			case "mixed-management":
				r.Header.Set(ManagementHeader, "")
			case "mixed-browser":
				r.Header.Set("X-Model-Center-Token", token)
			case "empty-browser":
				r.Header.Set("X-Model-Center-Token", "")
			case "duplicate-token":
				r.Header.Add(ReadOnlyHeader, token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}

func TestReadOnlyTokenCannotWriteOrInfer(t *testing.T) {
	h, token, _ := runtimeCapabilitiesFixture(t)
	for _, route := range []struct{ method, path string }{
		{"GET", BindingsPath}, {"PUT", BindingsPath + "/highlight.score"}, {"DELETE", BindingsPath + "/highlight.score"},
		{"POST", RuntimeAssetsPath}, {"POST", RuntimeJobsPath}, {"GET", RuntimeJobsPath + "/job-1"},
	} {
		for _, header := range []string{ReadOnlyHeader, RuntimeHeader, ManagementHeader} {
			r := httptest.NewRequest(route.method, "http://127.0.0.1:18120"+route.path, nil)
			r.Header.Set(header, token)
			if h.AuthorizeReadOnly(r) {
				t.Fatal("read token authorized non-read route")
			}
			for _, permission := range []SessionPermission{PermissionRuntime, PermissionManagement} {
				if _, err := h.AuthorizeSession(r, permission); err == nil {
					t.Fatal("read token gained session permission")
				}
			}
		}
	}
}
