package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeCenter(t *testing.T, extra ...func(*http.ServeMux)) (*Client, Descriptor) {
	t.Helper()
	const instance = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const build = "test-build"
	mux := http.NewServeMux()
	for _, f := range extra {
		f(mux)
	}
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("health request carried authorization")
		}
		_ = json.NewEncoder(w).Encode(Health{Schema: 1, Product: Product, Protocol: Protocol, Build: build, InstanceID: instance, State: "ready"})
	})
	mux.HandleFunc(DependenciesPath, func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(DependencySnapshot{Schema: 1, InstanceID: instance, Build: build, ObservedAt: now, FinishedAt: now})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p, err := queryProcess(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	root, data := t.TempDir(), t.TempDir()
	exe := filepath.Join(root, "model-center", "fengshen-model-center.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("fake center"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data}
	rootID, dataID, err := directoryIdentities(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Schema: 1, InstanceID: instance, PID: p.PID, ProcessStartedAt: p.Created, LoopbackAddress: strings.TrimPrefix(srv.URL, "http://"), Build: build, RootKind: "installed", RootIdentity: rootID, DataIdentity: dataID}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, DescriptorName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return &Client{cfg: cfg}, d
}

func TestClientInspectAndDependenciesWithoutToken(t *testing.T) {
	c, d := fakeCenter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := c.Inspect(ctx)
	if err != nil || status.InstanceID != d.InstanceID || status.Build != d.Build || status.HealthState != "ready" {
		t.Fatalf("inspect: %+v %v", status, err)
	}
	snapshot, err := c.Dependencies(ctx)
	if err != nil || snapshot.InstanceID != d.InstanceID || snapshot.Build != d.Build {
		t.Fatalf("dependencies: %+v %v", snapshot, err)
	}
}

func TestHostReadOnlyWithoutToken(t *testing.T) {
	dashboard := Dashboard{State: json.RawMessage(`{"catalog":[]}`), Inventory: json.RawMessage(`null`), LastCompleteInventory: json.RawMessage(`null`), Components: json.RawMessage(`[]`)}
	h := &Host{
		cfg: HostConfig{
			Build:     "test-build",
			Dashboard: func() (Dashboard, error) { return dashboard, nil },
			Report: func() (Report, error) {
				return Report{Dashboard: dashboard, ObservedAt: time.Now().UTC(), Models: json.RawMessage(`[]`)}, nil
			},
			ModelFiles: func(inventory, model, cursor string) (FilesPage, error) {
				return FilesPage{Files: json.RawMessage(`[]`)}, nil
			},
		},
		desc: Descriptor{InstanceID: strings.Repeat("b", 64), LoopbackAddress: "127.0.0.1:18120"},
		now:  time.Now,
	}
	paths := []string{HealthPath, DependenciesPath, DashboardPath, ReportPath, FilesPath + "?inventory_id=" + strings.Repeat("a", 32) + "&model_id=seed-vc"}
	for _, path := range paths {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18120"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:18120/api/state", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown path: %d", w.Code)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBindingsClientWithoutSession(t *testing.T) {
	client, _ := fakeCenter(t, func(mux *http.ServeMux) {
		mux.HandleFunc(BindingsPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(BindingSnapshot{SchemaVersion: 1, Revision: 1, Bindings: map[string]BindingView{}})
		})
		mux.HandleFunc(BindingsPath+"/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(BindingSnapshot{SchemaVersion: 1, Revision: 2, Bindings: map[string]BindingView{}})
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if snapshot, err := client.Bindings(ctx); err != nil || snapshot.Revision != 1 {
		t.Fatalf("bindings: %+v %v", snapshot, err)
	}
	selection := BindingSelection{
		ExpectedRevision: 1,
		InventoryID:      strings.Repeat("c", 32),
		Files:            []BindingFileSelection{{ModelID: "local-text", FileID: "file-1"}},
		Component:        ExecutionComponentRef{ID: "llama-runtime", Version: "v1", ManifestSHA256: strings.Repeat("d", 64), Protocol: "llama.cpp.server.http/b12182"},
		Device:           "cpu",
	}
	if snapshot, err := client.PutBinding(ctx, "highlight.score", selection); err != nil || snapshot.Revision != 2 {
		t.Fatalf("put binding: %+v %v", snapshot, err)
	}
	if snapshot, err := client.DeleteBinding(ctx, "highlight.score", 2); err != nil || snapshot.Revision != 2 {
		t.Fatalf("delete binding: %+v %v", snapshot, err)
	}
}

func TestSessionScopeIsLocalOnly(t *testing.T) {
	b := ExecutionBindingRef{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("a", 64)}
	g := SessionScope{SessionID: "session", InstanceID: "instance", TaskID: "task", Bindings: []ExecutionBindingRef{b}}
	if !g.Allows("task", b) || g.Allows("other", b) {
		t.Fatal("scope check")
	}
	raw, err := json.Marshal(g)
	if err != nil || !bytes.Contains(raw, []byte(`"bindings"`)) {
		t.Fatalf("scope marshal: %s %v", raw, err)
	}
	if !copyScope(g).Allows("task", b) {
		t.Fatal("scope copy")
	}
}

func TestRuntimeSessionAndSubmitWithoutToken(t *testing.T) {
	binding := ExecutionBindingRef{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("a", 64)}
	component := ExecutionComponentRef{ID: "whisper-stream", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "stream.v1"}
	body := RuntimeBody{Component: component, Transcribe: &RuntimeTranscribeInput{AssetID: "audio", Stream: true, EpisodeNumber: 1}}
	var sessionID, taskID string
	client, d := fakeCenter(t, func(mux *http.ServeMux) {
		mux.HandleFunc(RuntimeJobsPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			sessionID = r.Header.Get(RuntimeSessionHeader)
			taskID = r.Header.Get(RuntimeTaskHeader)
			var envelope struct {
				RequestID string              `json:"request_id"`
				Binding   ExecutionBindingRef `json:"binding"`
				Body      RuntimeBody         `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&envelope)
			frozen, _ := json.Marshal(struct {
				Binding ExecutionBindingRef
				Body    RuntimeBody
			}{envelope.Binding, envelope.Body})
			sum := sha256.Sum256(frozen)
			_ = json.NewEncoder(w).Encode(RuntimeSnapshot{ID: "job-1", SessionID: sessionID, TaskID: taskID, RequestID: envelope.RequestID, Binding: envelope.Binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1})
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.OpenRuntimeSession(ctx, "task-1", []string{binding.Scene}, []ExecutionBindingRef{binding})
	if err != nil {
		t.Fatal(err)
	}
	if session.Scope().InstanceID != d.InstanceID || !session.Scope().Allows("task-1", binding) {
		t.Fatal("session scope")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:18120/", nil)
	if err = client.SetSessionHeaders(session, req); err != nil || req.Header.Get(RuntimeSessionHeader) == "" || req.Header.Get(RuntimeTaskHeader) != "task-1" {
		t.Fatalf("headers: %v %q %q", err, req.Header.Get(RuntimeSessionHeader), req.Header.Get(RuntimeTaskHeader))
	}
	if _, err = client.RuntimeSessionAddress(session); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntimeClient(client)
	job, err := runtime.Submit(ctx, session, "request-1", binding, body)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.Snapshot().ID != "job-1" || sessionID == "" || taskID != "task-1" {
		t.Fatalf("submit: %+v %q %q", job, sessionID, taskID)
	}
	if err = client.HeartbeatRuntimeSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err = client.CloseRuntimeSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.String(), "runtime session") {
		t.Fatal("session string")
	}
}
