package componentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/coder/websocket"
)

func newClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func authorized(r *http.Request) bool { return true }

func TestNewRejectsAdditionalBases(t *testing.T) {
	for _, base := range []string{
		"https://127.0.0.1:1",
		"http://127.0.0.1:1/path",
		"http://127.0.0.1:1?x=1",
		"http://localhost:1",
		"http://192.0.2.1:1",
		"not-a-url",
	} {
		if _, err := New(base); err == nil {
			t.Fatalf("accepted base %q", base)
		}
	}
	valid, err := New("http://127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	if valid.BaseURL() != "http://127.0.0.1:1234" {
		t.Fatalf("client accessor: %q", valid.BaseURL())
	}
}

func TestHealthAndWaitHealthy(t *testing.T) {
	var calls int32
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) || r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.WaitHealthy(ctx); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("calls=%d", calls)
	}

	unhealthy := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := unhealthy.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "component_health_503") {
		t.Fatalf("health error=%v", err)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancelShort()
	if err := unhealthy.WaitHealthy(short); err == nil || !strings.Contains(err.Error(), "component_unavailable") {
		t.Fatalf("wait healthy error=%v", err)
	}
}

func TestJobsAndArtifacts(t *testing.T) {
	var jobCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			if r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
			var req componentprotocol.JobRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Kind != "whisper-batch" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(componentprotocol.JobSnapshot{ID: "j1", State: componentprotocol.JobRunning})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/j1":
			state := componentprotocol.JobRunning
			if atomic.AddInt32(&jobCalls, 1) >= 2 {
				state = componentprotocol.JobSucceeded
			}
			_ = json.NewEncoder(w).Encode(componentprotocol.JobSnapshot{ID: "j1", State: state})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/j1/cancel":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/j1/artifacts/result.bin":
			_, _ = w.Write([]byte("data"))
		case r.URL.Path == "/v1/jobs/plain":
			http.Error(w, "plain failure", http.StatusInternalServerError)
		case r.URL.Path == "/v1/jobs/bad", r.URL.Path == "/v1/jobs/bad/cancel":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(componentprotocol.Error{Code: "component_conflict", Message: "busy"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	req := componentprotocol.JobRequest{
		Protocol:  componentprotocol.BatchHTTPv1,
		Kind:      "whisper-batch",
		RequestID: "r1",
		Input:     componentprotocol.JobInput{AudioPath: `C:\fixture\audio.wav`},
		Models:    map[string][]string{"whisper-asr": {`C:\fixture\model.bin`}},
	}
	snap, err := client.SubmitJob(context.Background(), req)
	if err != nil || snap.ID != "j1" || snap.State != componentprotocol.JobRunning {
		t.Fatalf("submit=%+v %v", snap, err)
	}
	if _, err = client.Job(context.Background(), "j1"); err != nil {
		t.Fatal(err)
	}
	done, err := client.WaitJob(context.Background(), "j1")
	if err != nil || done.State != componentprotocol.JobSucceeded {
		t.Fatalf("wait=%+v %v", done, err)
	}
	if err = client.CancelJob(context.Background(), "j1"); err != nil {
		t.Fatal(err)
	}
	data, err := client.DownloadArtifact(context.Background(), "j1", "result.bin", 4)
	if err != nil || string(data) != "data" {
		t.Fatalf("artifact=%q %v", data, err)
	}
	if _, err = client.DownloadArtifact(context.Background(), "j1", "result.bin", 3); err == nil || err.Error() != "component_artifact_too_large" {
		t.Fatalf("oversized artifact error=%v", err)
	}
	var componentErr *componentprotocol.Error
	if _, err = client.Job(context.Background(), "bad"); !errors.As(err, &componentErr) || componentErr.Code != "component_conflict" {
		t.Fatalf("structured job error=%v", err)
	}
	if _, err = client.Job(context.Background(), "plain"); err == nil || err.Error() != "component_http_500" {
		t.Fatalf("plain job error=%v", err)
	}
	if err = client.CancelJob(context.Background(), "bad"); !errors.As(err, &componentErr) || componentErr.Code != "component_conflict" {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestInvalidResponses(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/jobs":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("{"))
		case "/v1/jobs/bad":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{"))
		default:
			http.NotFound(w, r)
		}
	})
	req := componentprotocol.JobRequest{Protocol: componentprotocol.BatchHTTPv1, Kind: "whisper-batch", RequestID: "r1", Input: componentprotocol.JobInput{AudioPath: `C:\fixture\audio.wav`}, Models: map[string][]string{"whisper-asr": {`C:\fixture\model.bin`}}}
	if _, err := client.SubmitJob(context.Background(), req); err == nil || err.Error() != "component_response_invalid" {
		t.Fatalf("submit invalid response=%v", err)
	}
	if _, err := client.Job(context.Background(), "bad"); err == nil || err.Error() != "component_response_invalid" {
		t.Fatalf("job invalid response=%v", err)
	}
}

func TestClassifyAndLlama(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/face/classify":
			if r.Header.Get("Content-Type") != "image/jpeg" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
			image, _ := io.ReadAll(r.Body)
			kind := string(image)
			if kind == "" {
				kind = "frontal"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"kind": kind})
		case "/v1/llama/start":
			w.WriteHeader(http.StatusOK)
		case "/health":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	for _, kind := range []string{"frontal", "profile", "no_face"} {
		if got, err := client.Classify(context.Background(), "image/jpeg", []byte(kind)); err != nil || got != kind {
			t.Fatalf("classify %s: %q %v", kind, got, err)
		}
	}
	if err := client.StartLlama(context.Background(), componentprotocol.LlamaStartRequest{Protocol: componentprotocol.LlamaHTTPv1, Model: `C:\fixture\model.gguf`}); err != nil {
		t.Fatal(err)
	}
	if err := client.LlamaHealth(context.Background()); err != nil {
		t.Fatal(err)
	}

	bad := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/face/classify":
			_ = json.NewEncoder(w).Encode(map[string]string{"kind": "other"})
		case "/v1/llama/start", "/health":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(componentprotocol.Error{Code: "component_failed"})
		}
	})
	if _, err := bad.Classify(context.Background(), "image/png", nil); err == nil {
		t.Fatal("invalid classify kind accepted")
	}
	if err := bad.StartLlama(context.Background(), componentprotocol.LlamaStartRequest{Protocol: componentprotocol.LlamaHTTPv1, Model: `C:\fixture\model.gguf`}); err == nil {
		t.Fatal("llama start failure accepted")
	}
	if err := bad.LlamaHealth(context.Background()); err == nil {
		t.Fatal("llama health failure accepted")
	}
}

func TestStream(t *testing.T) {
	extra := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if !authorized(r) {
			return
		}
		ctx := r.Context()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err = conn.Write(ctx, typ, data); err != nil {
			return
		}
		if extra {
			_ = conn.Write(ctx, websocket.MessageText, []byte("extra"))
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.DialStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err = stream.Send(ctx, map[string]string{"hello": "world"}); err != nil {
		t.Fatal(err)
	}
	data, err := stream.ReadRaw(ctx)
	if err != nil || !strings.Contains(string(data), "hello") {
		t.Fatalf("read=%q %v", data, err)
	}
	if err = stream.ExpectNormalClose(ctx); err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(ctx, make(chan int)); err == nil {
		t.Fatal("marshal failure accepted")
	}
	if err = stream.Send(ctx, strings.Repeat("x", componentprotocol.MaxWSFrame+1)); err == nil || err.Error() != "component_frame_too_large" {
		t.Fatalf("oversized frame error=%v", err)
	}

	extra = true
	second, err := client.DialStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err = second.Send(ctx, map[string]string{"hello": "world"}); err != nil {
		t.Fatal(err)
	}
	if _, err = second.ReadRaw(ctx); err != nil {
		t.Fatal(err)
	}
	if err = second.ExpectNormalClose(ctx); err == nil || err.Error() != "component_frame_extra" {
		t.Fatalf("extra frame error=%v", err)
	}
}
