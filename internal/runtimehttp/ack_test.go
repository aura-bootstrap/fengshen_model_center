package runtimehttp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

func TestAcknowledgeRequiresExplicitOwnedTerminalReceipt(t *testing.T) {
	h, auth, ex, _ := setup(t)
	s := submit(t, h, payload(auth, upload(t, h)))
	<-ex.start
	path := integration.RuntimeJobsPath + "/" + s.ID
	if w := request(h, http.MethodPost, path+"/ack", "application/json", []byte(`{}`)); w.Code != http.StatusConflict {
		t.Fatalf("running acknowledgement: %d", w.Code)
	}
	close(ex.release)
	s = waitTerminal(t, h, s)
	if s.State != runtimejobs.Succeeded {
		t.Fatal("job did not succeed")
	}
	for _, suffix := range []string{"", "/results/" + s.ID} {
		if w := request(h, http.MethodGet, path+suffix, "", nil); w.Code != http.StatusOK {
			t.Fatalf("read result: %d", w.Code)
		}
	}
	current, err := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
	if err != nil || current.Acknowledged {
		t.Fatal("read implicitly acknowledged result")
	}
	for _, change := range []string{"task", "session", "binding"} {
		auth.mu.Lock()
		saved := auth.grant
		switch change {
		case "task":
			auth.grant.TaskID = "other-task"
		case "session":
			auth.grant.SessionID = "other-session"
		case "binding":
			auth.grant.Bindings = []integration.ExecutionBindingRef{{Scene: "image.face", Revision: 2, SHA256: strings.Repeat("a", 64)}}
		}
		auth.mu.Unlock()
		w := request(h, http.MethodPost, path+"/ack", "application/json", []byte(`{}`))
		auth.mu.Lock()
		auth.grant = saved
		auth.mu.Unlock()
		if w.Code < 400 {
			t.Fatalf("cross-%s acknowledged: %d", change, w.Code)
		}
	}
	for _, body := range []string{``, `null`, `{"task_id":"task"}`, `{"ack":true}`, `{} {}`} {
		if w := request(h, http.MethodPost, path+"/ack", "application/json", []byte(body)); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("malformed acknowledgement: %d", w.Code)
		}
	}
	if w := request(h, http.MethodPost, path+"/ack", "text/plain", []byte(`{}`)); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("untyped acknowledgement: %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		w := request(h, http.MethodPost, path+"/ack", "application/json", []byte(`{}`))
		var got runtimejobs.Snapshot
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ID != s.ID || got.SessionID != s.SessionID || got.TaskID != s.TaskID || got.State != runtimejobs.Succeeded || !got.Acknowledged {
			t.Fatalf("explicit acknowledgement attempt %d: %d", i, w.Code)
		}
	}
	if ex.count.Load() != 1 {
		t.Fatal("acknowledgement replayed inference")
	}
}
