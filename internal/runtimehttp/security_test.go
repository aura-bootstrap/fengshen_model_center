package runtimehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

func TestDownloadRejectsMismatchedDigest(t *testing.T) {
	h, a, ex, _ := setup(t)
	a.grant.Capabilities = []string{"timbre.convert"}
	a.grant.Bindings[0].Scene = "timbre.convert"
	ex.media = true
	ex.badMeta = true
	s := submit(t, h, payload(a, upload(t, h)))
	<-ex.start
	close(ex.release)
	s = waitTerminal(t, h, s)
	result, e := h.jobs.Result(s.SessionID, s.TaskID, s.ID)
	if e != nil {
		t.Fatal(e)
	}
	w := request(h, "GET", integration.RuntimeJobsPath+"/"+s.ID+"/results/"+result.Media[0].ID, "", nil)
	if w.Code != 422 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal(w.Code, w.Body)
	}
}
func TestImageDimensionsAndTypeMismatch(t *testing.T) {
	h, _, _, _ := setup(t)
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewGray(image.Rect(0, 0, 4097, 1))); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		typ  string
		data []byte
	}{{"image/png", b.Bytes()}, {"image/jpeg", picture()}, {"image/png", wav()}} {
		if w := request(h, "POST", integration.RuntimeAssetsPath, tc.typ, tc.data); w.Code != 422 {
			t.Fatal(tc.typ, w.Code, w.Body)
		}
	}
}
func TestSessionExpiryCancelsBeforeReleasingAssets(t *testing.T) {
	h, a, ex, _ := setup(t)
	id := upload(t, h)
	s := submit(t, h, payload(a, id))
	<-ex.start
	h.ExpireSession(a.grant)
	current := waitTerminal(t, h, s)
	if current.State != runtimejobs.Cancelled || current.Code != "center_session_expired" {
		t.Fatal(current)
	}
	if _, _, err := h.assets.Open(context.Background(), s.TaskID, id); err != runtimeassets.ErrForbidden {
		t.Fatal(err)
	}
}
func TestTerminalFailureReplayAndConcurrentAdmission(t *testing.T) {
	h, a, ex, checks := setup(t)
	raw := payload(a, upload(t, h))
	const n = 8
	done := make(chan *httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		go func() { done <- request(h, "POST", integration.RuntimeJobsPath, "application/json", raw) }()
	}
	var s runtimejobs.Snapshot
	for i := 0; i < n; i++ {
		select {
		case w := <-done:
			if w.Code != 200 && w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			var got runtimejobs.Snapshot
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if s.ID != "" && s.ID != got.ID {
				t.Fatal("duplicate execution")
			}
			s = got
		case <-time.After(time.Second):
			t.Fatal("admission stuck")
		}
	}
	<-ex.start
	if checks.Load() != 1 || ex.count.Load() != 1 {
		t.Fatal(checks.Load(), ex.count.Load())
	}
	_, _ = h.jobs.Cancel(s.SessionID, s.TaskID, s.ID)
	h.validate = func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) error {
		t.Error("revalidated terminal job")
		return nil
	}
	w := request(h, "POST", integration.RuntimeJobsPath, "application/json", raw)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
}
func TestTrustedBoundaryChecksEveryRequest(t *testing.T) {
	h, a, _, _ := setup(t)
	for _, path := range []string{integration.RuntimeJobsPath, integration.RuntimeAssetsPath, integration.RuntimeJobsPath + "/unknown"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+path, bytes.NewReader(nil))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	a.mu.Lock()
	calls := a.calls
	a.mu.Unlock()
	if calls != 3 {
		t.Fatal(calls)
	}
}
