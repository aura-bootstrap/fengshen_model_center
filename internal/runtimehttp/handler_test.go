package runtimehttp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

type testAuth struct {
	mu    sync.Mutex
	grant integration.SessionScope
}

var activeAuth *testAuth

func (a *testAuth) snapshot() integration.SessionScope {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grant
}

type fakeExecutor struct {
	start   chan runtimejobs.Request
	release chan struct{}
	count   atomic.Int32
	burst   int
	store   *runtimeassets.Store
	media   bool
	badMeta bool
}

func (e *fakeExecutor) Execute(ctx context.Context, r runtimejobs.Request, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	e.count.Add(1)
	e.start <- r
	for i := 0; i < e.burst; i++ {
		if err := emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning, Completed: i, Total: e.burst}); err != nil {
			return runtimejobs.Result{}, err
		}
	}
	select {
	case <-ctx.Done():
		return runtimejobs.Result{}, ctx.Err()
	case <-e.release:
	}
	if e.media {
		data := wav()
		f, err := e.store.CreateResult(ctx, r.TaskID, r.SessionID, runtimeassets.WAV, int64(len(data)))
		if err != nil {
			return runtimejobs.Result{}, err
		}
		_, _ = f.Write(data)
		a, err := e.store.PublishResult(ctx, r.TaskID, f)
		if err != nil {
			return runtimejobs.Result{}, err
		}
		m := a.Metadata()
		if e.badMeta {
			m.SHA256 = strings.Repeat("c", 64)
		}
		return runtimejobs.Result{Component: r.Body.Component, Media: []runtimejobs.MediaResult{{ID: a.ID(), Bytes: m.Bytes, SHA256: m.SHA256, SampleRate: int(m.SampleRate), Channels: int(m.Channels), DurationMS: int64(m.DurationSeconds * 1000)}}}, nil
	}
	return runtimejobs.Result{Component: r.Body.Component, Face: runtimejobs.NoFace}, nil
}
func (*fakeExecutor) Close() error { return nil }
func setup(t *testing.T) (*Handler, *testAuth, *fakeExecutor, *atomic.Int32) {
	t.Helper()
	store, e := runtimeassets.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ex := &fakeExecutor{start: make(chan runtimejobs.Request, 128), release: make(chan struct{}), store: store}
	jobs, e := runtimejobs.New(ex, runtimejobs.Options{})
	if e != nil {
		t.Fatal(e)
	}
	b := integration.ExecutionBindingRef{Scene: "image.face", Revision: 1, SHA256: strings.Repeat("a", 64)}
	auth := &testAuth{grant: integration.SessionScope{SessionID: "session", TaskID: "task", Bindings: []integration.ExecutionBindingRef{b}}}
	activeAuth = auth
	checks := &atomic.Int32{}
	h := newHandler(store, jobs, func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) error {
		checks.Add(1)
		return nil
	})
	h.recheck = 10 * time.Millisecond
	t.Cleanup(func() { _ = h.Close() })
	return h, auth, ex, checks
}
func request(h *Handler, method, path, typ string, b []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:1234"+path, bytes.NewReader(b))
	addSession(r)
	g := activeAuth.snapshot()
	r.Header.Set(integration.RuntimeSessionHeader, g.SessionID)
	r.Header.Set(integration.RuntimeTaskHeader, g.TaskID)
	if typ != "" {
		r.Header.Set("Content-Type", typ)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func addSession(r *http.Request) {
	if activeAuth == nil {
		return
	}
	g := activeAuth.snapshot()
	r.Header.Set(integration.RuntimeSessionHeader, g.SessionID)
	r.Header.Set(integration.RuntimeTaskHeader, g.TaskID)
}

func picture() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}
func wav() []byte {
	b := make([]byte, 1644)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 1600)
	return b
}
func upload(t *testing.T, h *Handler) string {
	t.Helper()
	w := request(h, "POST", integration.RuntimeAssetsPath, "image/png", picture())
	if w.Code != 201 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	var a struct {
		ID string `json:"asset_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	return a.ID
}
func payload(a *testAuth, id string) []byte {
	a.mu.Lock()
	b := a.grant.Bindings[0]
	a.mu.Unlock()
	component := integration.ExecutionComponentRef{ID: "haar-consumer", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "test/v1"}
	body := runtimejobs.Body{Component: component, Face: &runtimejobs.FaceInput{AssetID: id}}
	if b.Scene == "timbre.convert" {
		body = runtimejobs.Body{Component: integration.ExecutionComponentRef{ID: "seedvc-runner", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "test/v1"}, Convert: &runtimejobs.ConvertInput{AssetID: id, ReferenceAssetID: id}}
	}
	raw, _ := json.Marshal(struct {
		RequestID string                          `json:"request_id"`
		Binding   integration.ExecutionBindingRef `json:"binding"`
		Body      runtimejobs.Body                `json:"body"`
	}{"request", b, body})
	return raw
}
func submit(t *testing.T, h *Handler, raw []byte) runtimejobs.Snapshot {
	t.Helper()
	w := request(h, "POST", integration.RuntimeJobsPath, "application/json", raw)
	if w.Code != 202 {
		t.Fatalf("submit %d %s", w.Code, w.Body)
	}
	var s runtimejobs.Snapshot
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	return s
}
func waitTerminal(t *testing.T, h *Handler, s runtimejobs.Snapshot) runtimejobs.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cursor := uint64(0)
	for {
		events, n, e := h.jobs.WaitEvents(ctx, s.SessionID, s.TaskID, s.ID, cursor)
		if e != nil {
			t.Fatal(e)
		}
		cursor = n
		for _, v := range events {
			if v.State.Terminal() {
				result, _ := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
				return result
			}
		}
	}
}
func TestIdempotencyOwnershipAndValidation(t *testing.T) {
	h, a, ex, checks := setup(t)
	id := upload(t, h)
	raw := payload(a, id)
	s := submit(t, h, raw)
	<-ex.start
	close(ex.release)
	s = waitTerminal(t, h, s)
	if s.State != runtimejobs.Succeeded {
		t.Fatal(s)
	}
	h.validate = func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) error {
		return errors.New("W:/token secret")
	}
	w := request(h, "POST", integration.RuntimeJobsPath, "application/json", raw)
	if w.Code != 200 || checks.Load() != 1 || ex.count.Load() != 1 {
		t.Fatalf("replay %d %s", w.Code, w.Body)
	}
	conflict := bytes.Replace(raw, []byte(`"version":"v1"`), []byte(`"version":"v2"`), 1)
	if w = request(h, "POST", integration.RuntimeJobsPath, "application/json", conflict); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	path := integration.RuntimeJobsPath + "/" + s.ID
	for _, change := range []string{"task", "session"} {
		a.mu.Lock()
		saved := a.grant
		switch change {
		case "task":
			a.grant.TaskID = "other"
		case "session":
			a.grant.SessionID = "other"
		}
		a.mu.Unlock()
		w = request(h, "GET", path, "", nil)
		if w.Code < 400 {
			t.Fatal(change, w.Code)
		}
		if change == "session" {
			w = request(h, "POST", integration.RuntimeJobsPath, "application/json", raw)
			if w.Code != 403 {
				t.Fatal("cross-session input", w.Code)
			}
		}
		a.mu.Lock()
		a.grant = saved
		a.mu.Unlock()
	}
	w = request(h, "GET", path+"/results/"+s.ID, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	after, _ := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
	if after.Acknowledged {
		t.Fatal("GET acknowledged")
	}
	w = request(h, "POST", integration.RuntimeJobsPath, "application/json", bytes.Replace(raw, []byte(`"request_id":"request"`), []byte(`"request_id":"new"`), 1))
	if w.Code != 409 || strings.Contains(w.Body.String(), "W:/") || strings.Contains(w.Body.String(), "token") {
		t.Fatal(w.Code, w.Body)
	}
}
func TestStrictRequests(t *testing.T) {
	h, a, _, _ := setup(t)
	id := upload(t, h)
	raw := payload(a, id)
	cases := [][]byte{
		append(append([]byte{}, raw...), []byte(` {}`)...),
		bytes.Replace(raw, []byte(`"request_id":"request"`), []byte(`"request_id":"request","request_id":"other"`), 1),
		bytes.Replace(raw, []byte(`"request_id"`), []byte(`"Request_ID"`), 1),
		bytes.Replace(raw, []byte(`"request_id":"request"`), []byte(`"request_id":null`), 1),
		bytes.Replace(raw, []byte(`"revision":1`), []byte(`"revision":1,"revision":2`), 1),
		bytes.Replace(raw, []byte(`"scene"`), []byte(`"Scene"`), 1),
		bytes.Replace(raw, []byte(`"asset_id"`), []byte(`"Asset_ID"`), 1),
		bytes.Replace(raw, []byte(`"face":`), []byte(`"unknown":`), 1),
		bytes.Replace(raw, []byte(`"face":`), []byte(`"face":null,"other":`), 1),
		bytes.Replace(raw, []byte(`"version":"v1"`), []byte{'"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '"', 255, '"'}, 1),
		[]byte(`null`), []byte(`{}`), bytes.Repeat([]byte(" "), runtimejobs.MaxBodyBytes+1025),
	}
	for i, b := range cases {
		w := request(h, "POST", integration.RuntimeJobsPath, "application/json", b)
		if w.Code < 400 {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func TestUploadAndHeaders(t *testing.T) {
	h, _, _, _ := setup(t)
	for _, tc := range []struct {
		typ  string
		data []byte
		code int
	}{
		{"audio/wav", wav(), 201}, {"image/png", picture(), 201}, {"audio/wav", []byte("bad"), 422}, {"image/png", []byte("bad"), 422}, {"image/gif", picture(), 415}, {"image/png; x=y", picture(), 415}, {"application/json", []byte(`{"url":"file:///secret"}`), 415},
	} {
		w := request(h, "POST", integration.RuntimeAssetsPath, tc.typ, tc.data)
		if w.Code != tc.code {
			t.Fatal(tc.typ, w.Code, w.Body)
		}
	}
	for _, tc := range []struct{ name, value string }{{"Content-Encoding", "gzip"}, {"Content-Disposition", "attachment; filename=x"}, {"Range", "bytes=0-"}, {"Last-Event-ID", "0"}} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, bytes.NewReader(picture()))
		addSession(r)
		r.Header.Set("Content-Type", "image/png")
		r.Header.Set(tc.name, tc.value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(tc.name)
		}
	}
	for _, n := range []int64{-1, 0, 8<<20 + 1, 1} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, bytes.NewReader(picture()))
		addSession(r)
		r.Header.Set("Content-Type", "image/png")
		r.ContentLength = n
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(n)
		}
	}
	for _, path := range []string{integration.RuntimeAssetsPath + "?x=1", integration.RuntimeAssetsPath + "?"} {
		if w := request(h, "POST", path, "image/png", picture()); w.Code < 400 {
			t.Fatal(path)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeAssetsPath, bytes.NewReader(picture()))
	addSession(r)
	r.Header["Content-Type"] = []string{"image/png", "image/jpeg"}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
}
func TestCancelDisconnectAndExpiry(t *testing.T) {
	h, a, ex, _ := setup(t)
	s := submit(t, h, payload(a, upload(t, h)))
	<-ex.start
	path := integration.RuntimeJobsPath + "/" + s.ID
	for _, b := range []string{"", `null`, `{"x":1}`, `{} {}`, `[]`} {
		w := request(h, "POST", path+"/cancel", "application/json", []byte(b))
		if w.Code < 400 {
			t.Fatal(b)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:1234"+path+"/events", nil).WithContext(ctx)
	addSession(r)
	h.ServeHTTP(httptest.NewRecorder(), r)
	current, _ := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
	if current.State.Terminal() {
		t.Fatal("disconnect cancelled work")
	}
	w := request(h, "POST", path+"/cancel", "application/json", []byte(`{}`))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	current = waitTerminal(t, h, s)
	if current.State != runtimejobs.Cancelled {
		t.Fatal(current)
	}
	w = request(h, "POST", path+"/cancel", "application/json", []byte(`{}`))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	h.ExpireSession(a.grant)
	if _, _, e := h.assets.Open(context.Background(), s.TaskID, "missing"); e == nil {
		t.Fatal("released")
	}
}
func TestSSECursorAndPeriodicAuthorization(t *testing.T) {
	h, a, ex, _ := setup(t)
	ex.burst = 2050
	s := submit(t, h, payload(a, upload(t, h)))
	<-ex.start
	// Wait for notifications rather than sleeping for executor progress.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cursor := uint64(0)
	for cursor < 2052 {
		_, next, e := h.jobs.WaitEvents(ctx, s.SessionID, s.TaskID, s.ID, cursor)
		if e != nil {
			var core *runtimejobs.Error
			if !errors.As(e, &core) || core.Status != 410 {
				t.Fatal(e)
			}
			current, err := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			next = current.Cursor
		}
		cursor = next
	}
	path := integration.RuntimeJobsPath + "/" + s.ID + "/events"
	for _, v := range []string{"0", "01", "+1", "-1", "18446744073709551616", ""} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:1234"+path, nil)
		addSession(r)
		r.Header.Set("Last-Event-ID", v)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 422
		if v == "0" {
			want = 410
		}
		if w.Code != want || w.Header().Get("Content-Type") == "text/event-stream" {
			t.Fatal(v, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:1234"+path, nil)
	addSession(r)
	r.Header.Set("Last-Event-ID", "2052")
	r.Header.Set(integration.RuntimeSessionHeader, "other")
	r.Header.Set(integration.RuntimeTaskHeader, "task")
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expired session stream stuck")
	}
	s, _ = h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
	if s.State.Terminal() {
		t.Fatal("stream revocation itself cancelled job")
	}
}
func TestMediaResultScope(t *testing.T) {
	h, a, ex, _ := setup(t)
	a.grant.Bindings[0].Scene = "timbre.convert"
	ex.media = true
	id := upload(t, h)
	s := submit(t, h, payload(a, id))
	<-ex.start
	close(ex.release)
	s = waitTerminal(t, h, s)
	if s.State != runtimejobs.Succeeded {
		t.Fatal(s)
	}
	result, e := h.jobs.Result(s.SessionID, s.TaskID, s.ID)
	if e != nil {
		t.Fatal(e)
	}
	path := integration.RuntimeJobsPath + "/" + s.ID + "/results/"
	w := request(h, "GET", path+result.Media[0].ID, "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), wav()) || w.Header().Get("X-Content-SHA256") == "" {
		t.Fatal(w.Code, w.Body)
	}
	for _, other := range []string{id, "unknown"} {
		if w = request(h, "GET", path+other, "", nil); w.Code != 404 {
			t.Fatal(other, w.Code)
		}
	}
	f, e := h.assets.CreateResult(context.Background(), s.TaskID, s.SessionID, runtimeassets.WAV, int64(len(wav())))
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write(wav())
	unrelated, e := h.assets.PublishResult(context.Background(), s.TaskID, f)
	if e != nil {
		t.Fatal(e)
	}
	if w = request(h, "GET", path+unrelated.ID(), "", nil); w.Code != 404 {
		t.Fatal("unlisted output downloaded", w.Code)
	}
}

type brokenFlusher struct {
	header http.Header
	writes int
}

func (w *brokenFlusher) Header() http.Header         { return w.header }
func (*brokenFlusher) WriteHeader(int)               {}
func (w *brokenFlusher) Write(b []byte) (int, error) { w.writes++; return len(b), nil }
func (*brokenFlusher) FlushError() error             { return io.ErrClosedPipe }
func TestFlushErrorStopsStream(t *testing.T) {
	h, a, ex, _ := setup(t)
	s := submit(t, h, payload(a, upload(t, h)))
	<-ex.start
	r := httptest.NewRequest("GET", "http://127.0.0.1:1234"+integration.RuntimeJobsPath+"/"+s.ID+"/events", nil)
	addSession(r)
	w := &brokenFlusher{header: http.Header{}}
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush failure ignored")
	}
	current, _ := h.jobs.Snapshot(s.SessionID, s.TaskID, s.ID)
	if current.State.Terminal() {
		t.Fatal("flush cancelled job")
	}
}
func TestNewFailsClosedWithoutDependencies(t *testing.T) {
	h := New(nil, nil, nil)
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234"+integration.RuntimeJobsPath, bytes.NewReader([]byte(`{}`)))
	addSession(r)
	r.Header.Set(integration.RuntimeSessionHeader, "session")
	r.Header.Set(integration.RuntimeTaskHeader, "task")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
