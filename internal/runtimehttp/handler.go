// Package runtimehttp transports runtime work; it owns no listener.
package runtimehttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

type validator func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) error
type receipt struct {
	id      string
	request []byte
}
type owner struct{ session, task string }
type uploadFlight struct {
	owner  owner
	cancel context.CancelFunc
}

const uploadTimeout = 2 * time.Minute

// Handler must be reused for the lifetime of its dedicated jobs and assets stores.
// Receipts are bounded and retained while the corresponding core job exists.
type Handler struct {
	assets   *runtimeassets.Store
	jobs     *runtimejobs.Manager
	validate validator
	mu       sync.Mutex
	receipts map[owner]map[string]receipt
	inputs   map[string]owner
	uploads  map[*uploadFlight]struct{}
	revoked  map[string]bool
	closed   bool
	recheck  time.Duration
}

// New validates frozen binding and component identities; requests are
// unauthenticated local calls carrying only non-secret session/task IDs.
func New(assets *runtimeassets.Store, jobs *runtimejobs.Manager, validate func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) error) *Handler {
	return newHandler(assets, jobs, validate)
}
func newHandler(assets *runtimeassets.Store, jobs *runtimejobs.Manager, validate validator) *Handler {
	return &Handler{assets: assets, jobs: jobs, validate: validate, receipts: map[owner]map[string]receipt{}, inputs: map[string]owner{}, uploads: map[*uploadFlight]struct{}{}, revoked: map[string]bool{}, recheck: time.Second}
}

// CapabilityEvidence is a passive in-process view, not a runtime HTTP route.
func (h *Handler) CapabilityEvidence(binding integration.ExecutionBindingRef, component integration.ExecutionComponentRef) (bool, *integration.InferenceEvidence) {
	if h == nil || h.jobs == nil {
		return false, nil
	}
	return h.jobs.CapabilityEvidence(binding, component)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Code string `json:"code"`
	}{code})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		fail(w, 500, "center_transport_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
func transportError(w http.ResponseWriter, err error) {
	var e *runtimejobs.Error
	if errors.As(err, &e) {
		// Never forward arbitrary error text, including errors supplied by validators.
		switch e.Code {
		case "center_request_invalid", "center_request_too_large", "center_request_conflict", "center_runtime_busy", "center_spool_full", "center_closed", "center_session_expired", "center_job_not_found", "center_result_unavailable", "center_job_not_terminal", "center_event_cursor_invalid", "center_event_cursor_expired":
			fail(w, e.Status, e.Code)
			return
		}
	}
	switch {
	case errors.Is(err, runtimeassets.ErrForbidden):
		fail(w, 403, "center_asset_forbidden")
	case errors.Is(err, runtimeassets.ErrFull):
		fail(w, 507, "center_spool_full")
	case errors.Is(err, runtimeassets.ErrInvalid):
		fail(w, 422, "center_result_invalid")
	default:
		fail(w, 500, "center_transport_failed")
	}
}
func sessionFrom(r *http.Request) (integration.SessionScope, error) {
	values := r.Header.Values(integration.RuntimeSessionHeader)
	tasks := r.Header.Values(integration.RuntimeTaskHeader)
	if len(values) != 1 || len(tasks) != 1 || !opaqueID(values[0]) || !opaqueID(tasks[0]) {
		return integration.SessionScope{}, errors.New("denied")
	}
	return integration.SessionScope{SessionID: values[0], TaskID: tasks[0]}, nil
}
func opaqueID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func emptyBody(r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 1))
	return e == nil && len(b) == 0
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	g, e := sessionFrom(r)
	if e != nil {
		fail(w, 403, "center_session_denied")
		return
	}
	if !integration.RuntimeRouteAllowed(r.Method, r.URL.Path) || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.TransferEncoding) != 0 || len(r.Header.Values("Content-Length")) > 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.Header.Values("Content-Disposition")) != 0 || len(r.Header.Values("Range")) != 0 || len(r.Header.Values("Last-Event-ID")) != 0 && !strings.HasSuffix(r.URL.Path, "/events") {
		fail(w, 400, "center_request_invalid")
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed || h.jobs == nil || h.assets == nil || h.validate == nil {
		fail(w, 503, "center_closed")
		return
	}
	if r.URL.Path == integration.RuntimeAssetsPath {
		h.upload(w, r, g)
		return
	}
	if r.URL.Path == integration.RuntimeJobsPath {
		h.submit(w, r, g)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, integration.RuntimeJobsPath+"/"), "/")
	s, e := h.jobs.Snapshot(g.SessionID, g.TaskID, parts[0])
	if e != nil {
		transportError(w, e)
		return
	}
	if r.Method == "GET" && !emptyBody(r) {
		fail(w, 422, "center_request_invalid")
		return
	}
	if len(parts) == 1 {
		writeJSON(w, 200, s)
		return
	}
	switch parts[1] {
	case "ack":
		if contentType(r) != "application/json" {
			fail(w, 415, "center_media_type_invalid")
			return
		}
		raw, err := readBounded(r, 64)
		if err != nil || strictObject(raw, map[string]bool{}) != nil {
			fail(w, 422, "center_request_invalid")
			return
		}
		if err = h.jobs.Acknowledge(g.SessionID, g.TaskID, s.ID); err != nil {
			transportError(w, err)
			return
		}
		s, err = h.jobs.Snapshot(g.SessionID, g.TaskID, s.ID)
		if err != nil {
			transportError(w, err)
			return
		}
		writeJSON(w, 200, s)
	case "cancel":
		raw, e := readBounded(r, 64)
		if e != nil || strictObject(raw, map[string]bool{}) != nil {
			fail(w, 422, "center_request_invalid")
			return
		}
		s, e = h.jobs.Cancel(g.SessionID, g.TaskID, s.ID)
		if e != nil {
			transportError(w, e)
			return
		}
		writeJSON(w, 200, s)
	case "events":
		h.events(w, r, g, s)
	case "results":
		h.result(w, r, g, s, parts[2])
	}
}
func readBounded(r *http.Request, n int64) ([]byte, error) {
	if r.ContentLength > n {
		return nil, errors.New("large")
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, n+1))
	if e != nil || int64(len(b)) > n {
		return nil, errors.New("invalid")
	}
	return b, nil
}
func contentType(r *http.Request) string {
	v := r.Header.Values("Content-Type")
	if len(v) != 1 {
		return ""
	}
	return v[0]
}
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, g integration.SessionScope) {
	var k runtimeassets.Kind
	var max int64
	switch contentType(r) {
	case "audio/wav":
		k = runtimeassets.WAV
		max = 1 << 30
	case "image/png", "image/jpeg":
		k = runtimeassets.Image
		max = 8 << 20
	default:
		fail(w, 415, "center_media_type_invalid")
		return
	}
	if r.ContentLength <= 0 {
		fail(w, 411, "center_length_required")
		return
	}
	if r.ContentLength > max {
		fail(w, 413, "center_request_too_large")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), uploadTimeout)
	defer cancel()
	flight := &uploadFlight{owner: owner{g.SessionID, g.TaskID}, cancel: cancel}
	h.mu.Lock()
	status, code := 0, ""
	sessionCount := 0
	for _, o := range h.inputs {
		if o.session == g.SessionID {
			sessionCount++
		}
	}
	for f := range h.uploads {
		if f.owner.session == g.SessionID {
			sessionCount++
		}
	}
	switch {
	case h.closed:
		status, code = 503, "center_closed"
	case h.revoked[g.SessionID]:
		status, code = 403, "center_session_expired"
	case len(h.inputs)+len(h.uploads) >= 1024 || sessionCount >= 32:
		status, code = 507, "center_spool_full"
	default:
		h.uploads[flight] = struct{}{}
	}
	h.mu.Unlock()
	if status != 0 {
		fail(w, status, code)
		return
	}
	defer func() {
		h.mu.Lock()
		delete(h.uploads, flight)
		h.mu.Unlock()
	}()

	// A server Body.Close can wait for Read. Interrupt the socket read first.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(uploadTimeout))
	bodyClosed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = controller.SetReadDeadline(time.Now())
		if r.Body != nil {
			_ = r.Body.Close()
		}
		close(bodyClosed)
	})
	defer func() {
		if !stop() {
			<-bodyClosed
		}
		_ = controller.SetReadDeadline(time.Time{})
	}()
	src := &uploadReader{src: bufio.NewReaderSize(r.Body, 16), typ: contentType(r)}
	a, e := h.assets.Upload(ctx, g.TaskID, g.SessionID, k, r.ContentLength, src)
	if e != nil {
		transportError(w, e)
		return
	}
	h.mu.Lock()
	closed, revoked := h.closed, h.revoked[g.SessionID]
	if !closed && !revoked {
		delete(h.uploads, flight)
		h.inputs[a.ID()] = flight.owner
	}
	h.mu.Unlock()
	if closed || revoked {
		// Revocation owns reclamation, including uploads that finished just before it.
		_ = h.assets.ReleaseSession(g.SessionID)
		fail(w, 403, "center_session_expired")
		return
	}
	writeJSON(w, 201, struct {
		ID   string                 `json:"asset_id"`
		Meta runtimeassets.Metadata `json:"metadata"`
	}{a.ID(), a.Metadata()})
}

// uploadReader validates the advertised type after the store reserves byte capacity.
type uploadReader struct {
	src     *bufio.Reader
	typ     string
	checked bool
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if !r.checked {
		prefix, _ := r.src.Peek(12)
		if r.typ == "image/png" && !bytes.HasPrefix(prefix, []byte("\x89PNG\r\n\x1a\n")) || r.typ == "image/jpeg" && !bytes.HasPrefix(prefix, []byte{0xff, 0xd8, 0xff}) || r.typ == "audio/wav" && (len(prefix) < 12 || string(prefix[:4]) != "RIFF" || string(prefix[8:12]) != "WAVE") {
			return 0, runtimeassets.ErrInvalid
		}
		r.checked = true
	}
	return r.src.Read(p)
}

// strictObject checks the envelope without weakening DecodeBody's nested schema.
func strictObject(raw []byte, fields map[string]bool) error {
	if !utf8.Valid(raw) {
		return errors.New("utf8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return errors.New("object")
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return e
		}
		k, ok := t.(string)
		if !ok || !fields[k] || seen[k] {
			return errors.New("field")
		}
		seen[k] = true
		var v json.RawMessage
		if e = d.Decode(&v); e != nil {
			return e
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("null")
		}
	}
	if _, e = d.Token(); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("trailing")
	}
	for k, required := range fields {
		if required && !seen[k] {
			return errors.New("missing")
		}
	}
	return nil
}
func (h *Handler) submit(w http.ResponseWriter, r *http.Request, g integration.SessionScope) {
	if contentType(r) != "application/json" {
		fail(w, 415, "center_media_type_invalid")
		return
	}
	raw, e := readBounded(r, runtimejobs.MaxBodyBytes+1024)
	if e != nil {
		fail(w, 413, "center_request_too_large")
		return
	}
	if strictObject(raw, map[string]bool{"request_id": true, "binding": true, "body": true}) != nil {
		fail(w, 422, "center_request_invalid")
		return
	}
	var req struct {
		RequestID string          `json:"request_id"`
		Binding   json.RawMessage `json:"binding"`
		Body      json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &req) != nil || strictObject(req.Binding, map[string]bool{"scene": true, "revision": true, "sha256": true}) != nil {
		fail(w, 422, "center_request_invalid")
		return
	}
	var binding integration.ExecutionBindingRef
	if json.Unmarshal(req.Binding, &binding) != nil || binding.Validate() != nil {
		fail(w, 422, "center_request_invalid")
		return
	}
	body, e := runtimejobs.DecodeBody(req.Body)
	if e != nil {
		transportError(w, e)
		return
	}
	// Canonical typed data matches the core's idempotency domain.
	canonical, _ := json.Marshal(struct {
		Binding integration.ExecutionBindingRef
		Body    runtimejobs.Body
	}{binding, body})
	own := owner{g.SessionID, g.TaskID}
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for o, rs := range h.receipts {
		for key, v := range rs {
			if _, err := h.jobs.Snapshot(o.session, o.task, v.id); err != nil {
				delete(rs, key)
			} else {
				count++
			}
		}
		if len(rs) == 0 {
			delete(h.receipts, o)
		}
	}
	if old, ok := h.receipts[own][req.RequestID]; ok {
		if !bytes.Equal(old.request, canonical) {
			fail(w, 409, "center_request_conflict")
			return
		}
		s, err := h.jobs.Snapshot(g.SessionID, g.TaskID, old.id)
		if err != nil {
			transportError(w, err)
			return
		}
		writeJSON(w, 200, s)
		return
	}
	if h.closed {
		fail(w, 503, "center_closed")
		return
	}
	if count >= 128 {
		fail(w, 507, "center_spool_full")
		return
	}
	ids := []string{}
	if body.Transcribe != nil {
		ids = append(ids, body.Transcribe.AssetID)
	}
	if body.Vision != nil {
		ids = append(ids, body.Vision.AssetID)
		if body.Vision.Candidate.TaskID != "" && body.Vision.Candidate.TaskID != g.TaskID {
			fail(w, 403, "center_asset_forbidden")
			return
		}
	}
	if body.Face != nil {
		ids = append(ids, body.Face.AssetID)
	}
	if body.Separate != nil {
		ids = append(ids, body.Separate.AssetID)
	}
	if body.Convert != nil {
		ids = append(ids, body.Convert.AssetID, body.Convert.ReferenceAssetID)
	}
	if body.Score != nil && body.Score.TaskID != "" && body.Score.TaskID != g.TaskID {
		fail(w, 403, "center_asset_forbidden")
		return
	}
	for _, id := range ids {
		if h.inputs[id] != own {
			fail(w, 403, "center_asset_forbidden")
			return
		}
		reader, _, err := h.assets.Open(r.Context(), g.TaskID, id)
		if err != nil {
			transportError(w, err)
			return
		}
		reader.Close()
	}
	if h.validate(r.Context(), binding, body.Component) != nil {
		fail(w, 409, "center_binding_stale")
		return
	}
	s, e := h.jobs.Submit(r.Context(), g.SessionID, g.TaskID, req.RequestID, binding, body)
	if e != nil {
		transportError(w, e)
		return
	}
	if h.receipts[own] == nil {
		h.receipts[own] = map[string]receipt{}
	}
	h.receipts[own][req.RequestID] = receipt{s.ID, canonical}
	writeJSON(w, 202, s)
}
func (h *Handler) result(w http.ResponseWriter, r *http.Request, g integration.SessionScope, s runtimejobs.Snapshot, id string) {
	result, e := h.jobs.Result(g.SessionID, g.TaskID, s.ID)
	if e != nil {
		transportError(w, e)
		return
	}
	// The job ID is the fixed JSON result ID and is allowed by integration routes.
	if id == s.ID {
		writeJSON(w, 200, result)
		return
	}
	for _, m := range result.Media {
		if m.ID != id {
			continue
		}
		reader, meta, e := h.assets.Download(r.Context(), g.TaskID, id)
		if e != nil {
			transportError(w, e)
			return
		}
		defer reader.Close()
		if meta.Kind != runtimeassets.WAV || meta.Bytes != m.Bytes || meta.SHA256 != m.SHA256 || int(meta.SampleRate) != m.SampleRate || int(meta.Channels) != m.Channels || math.Abs(meta.DurationSeconds*1000-float64(m.DurationMS)) > 1 {
			fail(w, 422, "center_result_invalid")
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", meta.Bytes)
		w.Header().Set("X-Content-SHA256", meta.SHA256)
		_, _ = io.Copy(w, reader)
		return
	}
	fail(w, 404, "center_result_not_found")
}
func (h *Handler) events(w http.ResponseWriter, r *http.Request, g integration.SessionScope, s runtimejobs.Snapshot) {
	var cursor uint64
	values := r.Header.Values("Last-Event-ID")
	if len(values) > 1 {
		fail(w, 422, "center_event_cursor_invalid")
		return
	}
	if len(values) == 1 {
		n, e := strconv.ParseUint(values[0], 10, 64)
		if e != nil || strconv.FormatUint(n, 10) != values[0] {
			fail(w, 422, "center_event_cursor_invalid")
			return
		}
		cursor = n
	}
	events, next, e := h.jobs.Events(g.SessionID, g.TaskID, s.ID, cursor)
	if e != nil {
		transportError(w, e)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(w)
	for {
		fresh, err := sessionFrom(r)
		if err != nil || fresh.SessionID != g.SessionID || fresh.TaskID != g.TaskID {
			return
		}
		for _, event := range events {
			raw, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: runtime\ndata: %s\n\n", event.Sequence, raw); err != nil {
				return
			}
		}
		if err = controller.Flush(); err != nil {
			return
		}
		cursor = next
		ctx, cancel := context.WithTimeout(r.Context(), h.recheck)
		events, next, e = h.jobs.WaitEvents(ctx, g.SessionID, g.TaskID, s.ID, cursor)
		cancel()
		if errors.Is(e, context.DeadlineExceeded) && r.Context().Err() == nil {
			events = nil
			next = cursor
			continue
		}
		if e != nil || len(events) == 0 {
			return
		}
	}
}

// ExpireSession releases work owned by a session. Work is revoked before
// its files are released, so late executors cannot publish a successful result.
func (h *Handler) ExpireSession(g integration.SessionScope) {
	h.mu.Lock()
	h.revoked[g.SessionID] = true
	for f := range h.uploads {
		if f.owner.session == g.SessionID {
			f.cancel()
		}
	}
	for id, o := range h.inputs {
		if o.session == g.SessionID {
			delete(h.inputs, id)
		}
	}
	for o := range h.receipts {
		if o.session == g.SessionID {
			delete(h.receipts, o)
		}
	}
	h.mu.Unlock()
	if h.jobs != nil {
		h.jobs.ExpireSession(g.SessionID)
	}
	if h.assets != nil {
		_ = h.assets.ReleaseSession(g.SessionID)
	}
}

// Close joins runtime work and releases its spool, never the independent Host.
func (h *Handler) Close() error {
	h.mu.Lock()
	h.closed = true
	for f := range h.uploads {
		f.cancel()
	}
	clear(h.inputs)
	h.mu.Unlock()
	var a, b error
	if h.jobs != nil {
		a = h.jobs.Close()
	}
	if h.assets != nil {
		b = h.assets.Close()
	}
	return errors.Join(a, b)
}
