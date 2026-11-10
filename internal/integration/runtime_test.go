package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeScenesAndAssets(t *testing.T) {
	scenes := RuntimeScenes()
	if len(scenes) != 6 {
		t.Fatalf("scenes=%d", len(scenes))
	}
	assets := map[string]bool{}
	for _, scene := range scenes {
		for _, id := range append(scene.RequiredModels, scene.OptionalModels...) {
			assets[id] = true
		}
	}
	if len(assets) != 12 {
		t.Fatalf("assets=%v", assets)
	}
	scenes[0].RequiredModels[0] = "changed"
	if RuntimeScenes()[0].RequiredModels[0] == "changed" {
		t.Fatal("mutable scene registry")
	}
	if _, ok := RuntimeScene("seed-whisper"); ok {
		t.Fatal("internal dependency exposed as scene")
	}
}

func TestRuntimePermissionMatrix(t *testing.T) {
	for _, permission := range []SessionPermission{PermissionReadOnly, PermissionManagement, PermissionRuntime, "", "admin"} {
		for _, path := range []string{RuntimeCapabilitiesPath, BindingsPath, "/api/bindings/highlight.score", RuntimeAssetsPath, RuntimeJobsPath, "/api/runtime/jobs/abc", "/api/runtime/jobs/abc/cancel", RuntimeSessionPath, ReportPath} {
			for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
				allowed := RuntimeRouteAllowed(permission, method, path)
				want := permission == PermissionReadOnly && method == "GET" && path == RuntimeCapabilitiesPath ||
					permission == PermissionManagement && (method == "GET" && path == BindingsPath || (method == "PUT" || method == "DELETE") && path == "/api/bindings/highlight.score") ||
					permission == PermissionRuntime && (method == "POST" && (path == RuntimeAssetsPath || path == RuntimeJobsPath || path == "/api/runtime/jobs/abc/cancel") || method == "GET" && path == "/api/runtime/jobs/abc" || method == "DELETE" && path == RuntimeSessionPath)
				if allowed != want {
					t.Errorf("%q %s %s=%t want %t", permission, method, path, allowed, want)
				}
			}
		}
	}
	for _, path := range []string{"/api/runtime/jobs/../abc", "/api/runtime/jobs/abc/", "/api/runtime/jobs/abc?token=x", "/api/runtime/jobs/abc/results/../../models", "/api/bindings/unknown"} {
		for _, permission := range []SessionPermission{PermissionReadOnly, PermissionManagement, PermissionRuntime} {
			if RuntimeRouteAllowed(permission, "GET", path) {
				t.Fatalf("unsafe route accepted: %s", path)
			}
		}
	}
}

func TestExecutionBindingRefValidation(t *testing.T) {
	valid := ExecutionBindingRef{Scene: "highlight.score", Revision: 1, SHA256: strings.Repeat("a", 64)}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ExecutionBindingRef){func(r *ExecutionBindingRef) { r.Scene = "seed-whisper" }, func(r *ExecutionBindingRef) { r.Revision = 0 }, func(r *ExecutionBindingRef) { r.SHA256 = strings.Repeat("A", 64) }, func(r *ExecutionBindingRef) { r.SHA256 = "" }} {
		bad := valid
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid ref: %+v", bad)
		}
	}
}

func TestWhisperVADSupported(t *testing.T) {
	for _, version := range []string{"", "unknown", "4979e04-batch-v1", "4979e04-batch-v2", "4979e04-batch-v20", "4979e04-batch-v2-extra", "4979E04-batch-v2"} {
		for _, id := range []string{"whisper-batch", "whisper-stream", "unknown"} {
			want := id == "whisper-stream" || id == "whisper-batch" && version == "4979e04-batch-v2"
			if got := WhisperVADSupported(ExecutionComponentRef{ID: id, Version: version}); got != want {
				t.Errorf("%s %q: %v want %v", id, version, got, want)
			}
		}
	}
}

func TestExecutionComponentVersion(t *testing.T) {
	ref := ExecutionComponentRef{ID: "llama-runtime", Version: "v1.2.3", ManifestSHA256: strings.Repeat("a", 64), Protocol: "fengshen.llama/v1"}
	if err := ref.Validate("highlight.score"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", ".", "..", "../v1", `v1\test`, "v1:2", "v1 ", "CON", "a..b"} {
		ref.Version = version
		if ref.Validate("highlight.score") == nil {
			t.Fatalf("unsafe component version accepted: %q", version)
		}
	}
}

func TestCapabilityDoesNotInventInference(t *testing.T) {
	value := RuntimeCapability{Scene: "highlight.score", State: CapabilityAvailable}
	if value.Validate() == nil {
		t.Fatal("available without binding/component accepted")
	}
	value.Binding = &ExecutionBindingRef{Scene: value.Scene, Revision: 1, SHA256: strings.Repeat("a", 64)}
	value.Component = &ExecutionComponentRef{ID: "llama-runtime", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "fengshen.llama/v1"}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Inference = &InferenceEvidence{State: "passed"}
	if value.Validate() == nil {
		t.Fatal("invented inference accepted")
	}
	value.Inference = nil
	value.State = "ready"
	if value.Validate() == nil {
		t.Fatal("unknown readiness accepted")
	}
}

func TestRuntimeProtocolDoesNotChangeReadOnlyReport(t *testing.T) {
	raw, err := json.Marshal(handshakeRequest{Schema: 1, Protocol: Protocol, InstanceID: "i", RootKind: "development", RootIdentity: "r", DataIdentity: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"schema":1,"protocol":"fengshen.model-center.integration/v1","instance_id":"i","root_kind":"development","root_identity":"r","data_identity":"d"}` {
		t.Fatalf("old handshake changed: %s", raw)
	}
	if RuntimeProtocol == Protocol || RuntimeHeader == ReadOnlyHeader {
		t.Fatal("runtime reuses read-only identity")
	}
}
