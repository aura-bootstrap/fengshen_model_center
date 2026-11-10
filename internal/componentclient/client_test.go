package componentclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/coder/websocket"
)

func TestNewRejectsInvalidBase(t *testing.T) {
	for _, base := range []string{"http://10.0.0.1:1", "https://127.0.0.1:1", "http://127.0.0.1:1/path", "http://localhost:1"} {
		if _, err := New(base); err == nil {
			t.Fatalf("accepted base %q", base)
		}
	}
}

func TestClientHealthJobsArtifacts(t *testing.T) {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc { return next }
	mux.HandleFunc("/healthz", auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	mux.HandleFunc("/v1/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"j1","seq":1,"state":"running"}`))
	}))
	mux.HandleFunc("/v1/jobs/j1", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"j1","seq":2,"state":"succeeded","artifacts":["a.bin"]}`))
	}))
	mux.HandleFunc("/v1/jobs/j1/artifacts/a.bin", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c, err := New(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := c.SubmitJob(ctx, componentprotocol.JobRequest{})
	if err != nil || snap.ID != "j1" {
		t.Fatal(err, snap)
	}
	snap, err = c.WaitJob(ctx, "j1")
	if err != nil || snap.State != componentprotocol.JobSucceeded {
		t.Fatal(err, snap)
	}
	data, err := c.DownloadArtifact(ctx, "j1", "a.bin", 16)
	if err != nil || string(data) != "data" {
		t.Fatal(err, string(data))
	}
	if _, err := c.DownloadArtifact(ctx, "j1", "a.bin", 2); err == nil {
		t.Fatal("artifact limit not enforced")
	}
}

func TestClientStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, append([]byte(`{"echo":`), append(data, '}')...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer ts.Close()
	c, err := New(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.DialStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send(ctx, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ReadRaw(ctx)
	if err != nil || !strings.Contains(string(raw), `"echo"`) {
		t.Fatal(err, string(raw))
	}
	if err := s.ExpectNormalClose(ctx); err != nil {
		t.Fatal(err)
	}
}
