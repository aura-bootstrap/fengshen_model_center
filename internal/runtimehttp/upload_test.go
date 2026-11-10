package runtimehttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
)

// gatedUploadBody blocks either before the prefix or after a valid prefix.
type gatedUploadBody struct {
	prefix    *bytes.Reader
	entered   chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (b *gatedUploadBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *gatedUploadBody) Close() error { b.closeOnce.Do(func() { close(b.closed) }); return nil }
func timely(t *testing.T, done <-chan struct{}, what string) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		t.Error(what + " blocked")
		return false
	}
}
func TestUploadReservesGlobalBytesBeforePrefix(t *testing.T) {
	h, a, _, _ := setup(t)
	var bodies []*gatedUploadBody
	var pending []chan struct{}
	defer func() {
		for _, b := range bodies {
			_ = b.Close()
		}
		for _, done := range pending {
			timely(t, done, "capacity cleanup")
		}
	}()
	for i := 0; i < 4; i++ {
		a.mu.Lock()
		a.grant.SessionID = "bytes-" + strconv.Itoa(i)
		a.mu.Unlock()
		b := &gatedUploadBody{prefix: bytes.NewReader(nil), entered: make(chan struct{}), closed: make(chan struct{})}
		bodies = append(bodies, b)
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, nil)
		addSession(r)
		r.Body = b
		r.ContentLength = 1 << 30
		r.Header.Set("Content-Type", "audio/wav")
		done := make(chan struct{})
		pending = append(pending, done)
		go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
		if !timely(t, b.entered, "byte reservation") {
			return
		}
	}
	a.mu.Lock()
	a.grant.SessionID = "bytes-overflow"
	a.mu.Unlock()
	if w := request(h, "POST", integration.RuntimeAssetsPath, "audio/wav", wav()); w.Code != 507 {
		t.Fatal("global byte limit relaxed", w.Code)
	}
}

func TestUploadCapacityIncludesFlights(t *testing.T) {
	for _, scope := range []string{"session", "global"} {
		t.Run(scope, func(t *testing.T) {
			h, a, _, _ := setup(t)
			g := a.grant
			count := 31
			if scope == "global" {
				count = 1023
			}
			h.mu.Lock()
			for i := 0; i < count; i++ {
				o := owner{g.SessionID, g.TaskID}
				if scope == "global" {
					o.session = "other"
				}
				h.inputs[strconv.Itoa(i)] = o
			}
			h.mu.Unlock()
			b := &gatedUploadBody{prefix: bytes.NewReader(nil), entered: make(chan struct{}), closed: make(chan struct{})}
			r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, nil)
			addSession(r)
			r.Body = b
			r.ContentLength = int64(len(picture()))
			r.Header.Set("Content-Type", "image/png")
			done := make(chan struct{})
			go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
			defer func() { _ = b.Close(); timely(t, done, "cleanup") }()
			if !timely(t, b.entered, "reserved upload") {
				return
			}
			response := request(h, "POST", integration.RuntimeAssetsPath, "image/png", picture())
			if response.Code != 507 {
				t.Fatal("in-flight capacity not counted", response.Code)
			}
			h.ExpireSession(g)
			if !timely(t, done, "revocation") {
				return
			}
			if response = request(h, "POST", integration.RuntimeAssetsPath, "image/png", picture()); response.Code != 403 {
				t.Fatal("revoked owner admitted", response.Code)
			}
		})
	}
}

func TestUploadCancellationReclaimsReservation(t *testing.T) {
	h, _, _, _ := setup(t)
	root := t.TempDir()
	store, err := runtimeassets.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.assets.Close()
	h.assets = store
	ctx, cancel := context.WithCancel(context.Background())
	b := &gatedUploadBody{prefix: bytes.NewReader(picture()[:16]), entered: make(chan struct{}), closed: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, nil).WithContext(ctx)
	addSession(r)
	r.Body = b
	r.ContentLength = int64(len(picture()))
	r.Header.Set("Content-Type", "image/png")
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() { h.ServeHTTP(w, r); close(done) }()
	defer func() { cancel(); _ = b.Close(); timely(t, done, "cleanup") }()
	if !timely(t, b.entered, "body read") {
		return
	}
	children, err := os.ReadDir(root)
	if err != nil || len(children) != 1 {
		t.Fatalf("spool: %v %v", children, err)
	}
	dir := filepath.Join(root, children[0].Name())
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("missing reservation: %v %v", files, err)
	}
	cancel()
	if !timely(t, done, "request cancellation") {
		return
	}
	files, err = os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("unpublished file retained: %v %v", files, err)
	}
	if w.Code == 201 {
		t.Fatal("cancelled upload published")
	}
	h.mu.Lock()
	pending := len(h.uploads)
	h.mu.Unlock()
	if pending != 0 {
		t.Fatal("reservation retained", pending)
	}
	for i := 0; i < 32; i++ {
		upload(t, h)
	}
	if response := request(h, "POST", integration.RuntimeAssetsPath, "image/png", picture()); response.Code != 507 {
		t.Fatal("session capacity relaxed", response.Code)
	}
}

type gatedUploadResponse struct {
	*httptest.ResponseRecorder
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	deadline time.Time
}

func (w *gatedUploadResponse) SetReadDeadline(d time.Time) error {
	if w.deadline.IsZero() {
		w.deadline = d
	}
	return nil
}
func (w *gatedUploadResponse) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(p)
}
func TestUploadResponseDoesNotHoldGlobalLock(t *testing.T) {
	h, a, _, _ := setup(t)
	w := &gatedUploadResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, bytes.NewReader(picture()))
	addSession(r)
	r.Header.Set("Content-Type", "image/png")
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	defer func() { close(w.release); timely(t, done, "response cleanup") }()
	if !timely(t, w.entered, "response write") {
		return
	}
	if w.deadline.IsZero() || time.Until(w.deadline) > uploadTimeout {
		t.Error("missing bounded read deadline")
	}
	expired := make(chan struct{})
	go func() { h.ExpireSession(a.grant); close(expired) }()
	timely(t, expired, "expiry during response")
}

func TestStalledUploadDoesNotBlockRuntime(t *testing.T) {
	for _, prefix := range []int{0, 16} {
		for _, action := range []string{"other-session", "expire", "close"} {
			t.Run(action+"/prefix-"+strconv.Itoa(prefix), func(t *testing.T) {
				h, a, _, _ := setup(t)
				g := a.grant
				b := &gatedUploadBody{prefix: bytes.NewReader(picture()[:prefix]), entered: make(chan struct{}), closed: make(chan struct{})}
				r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, nil)
				addSession(r)
				r.Body = b
				r.ContentLength = int64(len(picture()))
				r.Header.Set("Content-Type", "image/png")
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { h.ServeHTTP(w, r); close(done) }()
				defer func() { _ = b.Close(); timely(t, done, "upload cleanup") }()
				if !timely(t, b.entered, "body read") {
					return
				}
				actionDone := make(chan struct{})
				go func() {
					switch action {
					case "other-session":
						a.mu.Lock()
						a.grant.SessionID = "other"
						a.grant.TaskID = "other-task"
						a.mu.Unlock()
						response := request(h, "POST", integration.RuntimeAssetsPath, "image/png", picture())
						if response.Code != 201 {
							t.Errorf("other session: %d", response.Code)
						}
					case "expire":
						h.ExpireSession(g)
					case "close":
						_ = h.Close()
					}
					close(actionDone)
				}()
				ok := timely(t, actionDone, action)
				if !ok {
					_ = b.Close()
					timely(t, actionDone, "action cleanup")
					return
				}
				if action == "other-session" {
					h.ExpireSession(g)
				}
				if !timely(t, b.closed, "body cancellation") {
					return
				}
				if !timely(t, done, "cancelled upload") {
					return
				}
				if w.Code == 201 {
					t.Error("revoked upload published")
				}
				h.mu.Lock()
				for _, o := range h.inputs {
					if o.session == g.SessionID {
						t.Error("revoked input retained")
					}
				}
				h.mu.Unlock()
			})
		}
	}
}
