//go:build windows

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type runtimeTestTransport func(*http.Request) (*http.Response, error)

func (f runtimeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeClientFixture(t *testing.T, scenes ...string) (*RuntimeClient, *SessionCredentials) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("FENGSHEN_SESSION_ENGINE", exe)
	t.Setenv("FENGSHEN_SESSION_ENGINE_SHA256", deliveryDigest(raw))
	c := sessionFixture(t)
	scene := "image.face"
	if len(scenes) > 0 {
		scene = scenes[0]
	}
	b := ExecutionBindingRef{Scene: scene, Revision: 1, SHA256: strings.Repeat("a", 64)}
	s, e := c.OpenRuntimeSession(context.Background(), "task-1", []string{b.Scene}, []ExecutionBindingRef{b})
	if e != nil {
		t.Fatal(e)
	}
	r := NewRuntimeClient(c)
	t.Cleanup(r.CloseIdleConnections)
	return r, s
}
func runtimeTestJSON(status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, ContentLength: int64(len(raw)), Body: io.NopCloser(bytes.NewReader(raw))}
}
func runtimeTestBody() RuntimeBody {
	return RuntimeBody{Component: ExecutionComponentRef{ID: "haar-consumer", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "haar/v1"}, Face: &RuntimeAssetInput{AssetID: "asset"}}
}
func TestRuntimeClientAuthenticatedTransport(t *testing.T) {
	c, s := runtimeClientFixture(t)
	ctx := context.Background()
	body := runtimeTestBody()
	binding := s.Grant().Bindings[0]
	calls := 0
	var snap RuntimeSnapshot
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get(RuntimeHeader) == "" || r.URL.Host != s.descriptor.LoopbackAddress || r.URL.RawQuery != "" {
			t.Fatal("not authenticated")
		}
		if r.GetBody != nil {
			t.Fatal("write replay enabled")
		}
		switch r.URL.Path {
		case RuntimeJobsPath:
			raw, e := io.ReadAll(r.Body)
			if e != nil || int64(len(raw)) != r.ContentLength {
				t.Fatal("body length", e)
			}
			var envelope struct {
				RequestID string              `json:"request_id"`
				Binding   ExecutionBindingRef `json:"binding"`
				Body      RuntimeBody         `json:"body"`
			}
			if runtimeDecode(raw, &envelope) != nil || envelope.RequestID != "request-1" {
				t.Fatal("wire")
			}
			frozen, _ := json.Marshal(struct {
				Binding ExecutionBindingRef
				Body    RuntimeBody
			}{envelope.Binding, envelope.Body})
			sum := sha256.Sum256(frozen)
			snap = RuntimeSnapshot{ID: "job-1", SessionID: s.Grant().SessionID, TaskID: "task-1", RequestID: envelope.RequestID, Binding: envelope.Binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1}
			return runtimeTestJSON(202, snap), nil
		case RuntimeJobsPath + "/job-1":
			next := snap
			next.State = "succeeded"
			next.FinishedAt = next.CreatedAt.Add(time.Second)
			next.Cursor = 4
			return runtimeTestJSON(200, next), nil
		case RuntimeJobsPath + "/job-1/results/job-1":
			return runtimeTestJSON(200, RuntimeResult{Component: body.Component, Face: RuntimeFrontal}), nil
		case RuntimeJobsPath + "/job-1/cancel":
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != "{}" {
				t.Fatal("cancel body")
			}
			return runtimeTestJSON(200, snap), nil
		default:
			t.Fatal("unexpected route", r.URL.Path)
			return nil, errors.New("unexpected")
		}
	})
	job, e := c.Submit(ctx, s, "request-1", binding, body)
	if e != nil {
		t.Fatal(e)
	}
	body.Face.AssetID = "mutated" // Receipt must own the original request.
	result, e := c.Result(ctx, s, job)
	if e != nil || result.Face != RuntimeFrontal {
		t.Fatal("result", e)
	}
	if _, e = c.Cancel(ctx, s, job); e != nil {
		t.Fatal(e)
	}
	if calls != 4 {
		t.Fatal("unexpected request count", calls)
	}
	old := calls
	s.mu.Lock()
	s.grant.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, e = c.Query(ctx, s, job); e == nil || calls != old {
		t.Fatal("expired credential reached transport")
	}
}
func TestRuntimeAckExplicitIdempotentStrictAndScoped(t *testing.T) {
	c, s := runtimeClientFixture(t)
	body := runtimeTestBody()
	binding := s.Grant().Bindings[0]
	raw, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{binding, body})
	h := sha256.Sum256(raw)
	snap := RuntimeSnapshot{ID: "job", SessionID: s.Grant().SessionID, TaskID: s.Grant().TaskID, RequestID: "request", Binding: binding, BodySHA256: hex.EncodeToString(h[:]), State: "succeeded", CreatedAt: time.Now().UTC(), Cursor: 4}
	snap.FinishedAt = snap.CreatedAt.Add(time.Second)
	j, e := c.Resume(s, snap, body)
	if e != nil {
		t.Fatal(e)
	}
	acks, reads := 0, 0
	mode := "good"
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == RuntimeJobsPath+"/job/ack" {
			acks++
			v, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || string(v) != "{}" || r.ContentLength != 2 || r.GetBody != nil || r.Header.Get("Content-Type") != "application/json" || r.Header.Get(RuntimeHeader) == "" {
				t.Fatal("Ack wire")
			}
			next := snap
			next.Acknowledged = true
			switch mode {
			case "task":
				next.TaskID = "other"
			case "session":
				next.SessionID = "other"
			case "job":
				next.ID = "other"
			case "state":
				next.State = "failed"
			case "unacked":
				next.Acknowledged = false
			case "unknown":
				return nil, errors.New("connection lost")
			}
			resp := runtimeTestJSON(200, next)
			if mode == "metadata" {
				resp.Header.Set("Content-Type", "text/plain")
			}
			return resp, nil
		}
		reads++
		if r.Method != "GET" {
			t.Fatal("implicit write")
		}
		if strings.Contains(r.URL.Path, "/results/") {
			return runtimeTestJSON(200, RuntimeResult{Component: body.Component, Face: RuntimeFrontal}), nil
		}
		return runtimeTestJSON(200, snap), nil
	})
	if _, e := c.Result(context.Background(), s, j); e != nil || acks != 0 || reads != 2 {
		t.Fatal("GET acknowledged", e)
	}
	for i := 0; i < 2; i++ {
		if _, e := c.Acknowledge(context.Background(), s, j); e != nil {
			t.Fatal("idempotent Ack", e)
		}
	}
	for _, bad := range []string{"task", "session", "job", "state", "unacked", "metadata", "unknown"} {
		mode = bad
		before := acks
		if _, e := c.Acknowledge(context.Background(), s, j); e == nil || acks != before+1 {
			t.Fatal("invalid/retried Ack", bad, e)
		}
	}
	before := acks
	for _, mutate := range []func(*RuntimeSnapshot){func(v *RuntimeSnapshot) { v.TaskID = "other" }, func(v *RuntimeSnapshot) { v.SessionID = "other" }} {
		other := *j
		mutate(&other.snapshot)
		if _, e := c.Acknowledge(context.Background(), s, &other); e == nil || acks != before {
			t.Fatal("cross-scope Ack reached transport")
		}
	}
}

func TestRuntimeStreamOptionsSubmitResumeAndSnapshotHash(t *testing.T) {
	c, s := runtimeClientFixture(t, "highlight.transcribe")
	binding := s.Grant().Bindings[0]
	body := RuntimeBody{Component: ExecutionComponentRef{ID: "whisper-stream", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "fengshen.whispercpp.stream/v1"}, Transcribe: &RuntimeTranscribeInput{AssetID: "audio", Stream: true, EpisodeNumber: 1, Options: &RuntimeStreamOptions{WindowSec: 40, StepSec: 1, StableRounds: 3, TimestampToleranceSec: .4, FinalizeDelaySec: 4}}}
	calls := 0
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, err := io.ReadAll(r.Body)
		var envelope struct {
			RequestID string              `json:"request_id"`
			Binding   ExecutionBindingRef `json:"binding"`
			Body      RuntimeBody         `json:"body"`
		}
		if err != nil || runtimeDecode(raw, &envelope) != nil || envelope.Body.Transcribe.Options == nil {
			t.Fatal("options absent from submit wire", err)
		}
		frozen, _ := json.Marshal(struct {
			Binding ExecutionBindingRef
			Body    RuntimeBody
		}{envelope.Binding, envelope.Body})
		sum := sha256.Sum256(frozen)
		return runtimeTestJSON(202, RuntimeSnapshot{ID: "job", SessionID: s.Grant().SessionID, TaskID: s.Grant().TaskID, RequestID: envelope.RequestID, Binding: binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1}), nil
	})
	job, err := c.Submit(context.Background(), s, "request", binding, body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(job.Snapshot())
	var snapshot RuntimeSnapshot
	if runtimeDecode(raw, &snapshot) != nil {
		t.Fatal("snapshot roundtrip")
	}
	resumed, err := c.Resume(s, snapshot, body)
	if err != nil || resumed.body.Transcribe.Options == body.Transcribe.Options {
		t.Fatal("resume did not own frozen options", err)
	}
	mutations := []func(*RuntimeStreamOptions){
		func(o *RuntimeStreamOptions) { o.WindowSec = 50 },
		func(o *RuntimeStreamOptions) { o.StepSec = 2 },
		func(o *RuntimeStreamOptions) { o.StableRounds = 4 },
		func(o *RuntimeStreamOptions) { o.TimestampToleranceSec = .5 },
		func(o *RuntimeStreamOptions) { o.FinalizeDelaySec = 5 },
	}
	original := *body.Transcribe.Options
	for _, mutate := range mutations {
		*body.Transcribe.Options = original
		mutate(body.Transcribe.Options)
		if _, err := c.Resume(s, snapshot, body); err == nil {
			t.Fatal("resume accepted changed options")
		}
		next, err := c.Submit(context.Background(), s, "other", binding, body)
		if err != nil || next.Snapshot().BodySHA256 == snapshot.BodySHA256 {
			t.Fatal("option not covered by hash", err)
		}
	}
	if *job.body.Transcribe.Options != original || *resumed.body.Transcribe.Options != original {
		t.Fatal("caller mutated admitted options")
	}
	body.Transcribe.Options = nil
	if _, err := c.Resume(s, snapshot, body); err == nil {
		t.Fatal("resume accepted missing options")
	}
	if calls != 6 {
		t.Fatal("resume performed network IO", calls)
	}
}

func TestRuntimeClientNoWriteRetryOrBlindCancel(t *testing.T) {
	c, s := runtimeClientFixture(t)
	calls := 0
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("network contains secret text")
	})
	_, e := c.Submit(context.Background(), s, "request-1", s.Grant().Bindings[0], runtimeTestBody())
	if e == nil || e.Error() != "center_transport_failed" || calls != 1 {
		t.Fatal("uncertain write replay/leak", calls, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = c.Submit(ctx, s, "request-1", s.Grant().Bindings[0], runtimeTestBody())
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestRuntimeClientUploadLengthDigestAndIdentity(t *testing.T) {
	c, s := runtimeClientFixture(t)
	data := []byte("fixture-image")
	sum := sha256.Sum256(data)
	mode := "good"
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(RuntimeHeader) == "" {
			t.Fatal("upload unauthenticated")
		}
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			return nil, e
		}
		m := RuntimeAssetMetadata{Kind: "image", Bytes: "13", SHA256: hex.EncodeToString(sum[:]), Width: 1, Height: 1}
		if mode == "digest" {
			m.SHA256 = strings.Repeat("0", 64)
		}
		if mode == "size" {
			m.Bytes = "12"
		}
		if mode == "dimensions" {
			m.Width = 4097
		}
		if len(raw) != 13 {
			t.Fatal("upload size")
		}
		return runtimeTestJSON(201, RuntimeAsset{ID: "asset", Metadata: m}), nil
	})
	for _, m := range []string{"good", "digest", "size", "dimensions"} {
		mode = m
		_, e := c.Upload(context.Background(), s, "image/png", 13, bytes.NewReader(data))
		if (e == nil) != (m == "good") {
			t.Fatal(m, e)
		}
	}
	mode = "good"
	if _, e := c.Upload(context.Background(), s, "image/png", 13, strings.NewReader("fixture-image-extra")); e == nil {
		t.Fatal("extra source bytes accepted")
	}
	if _, e := c.Upload(context.Background(), s, "image/png", 13, strings.NewReader("short")); e == nil {
		t.Fatal("truncated source accepted")
	}
}
func TestRuntimeClientRejectsCrossTaskAndMalformedResponses(t *testing.T) {
	c, s := runtimeClientFixture(t)
	body := runtimeTestBody()
	binding := s.Grant().Bindings[0]
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{binding, body})
	sum := sha256.Sum256(frozen)
	snap := RuntimeSnapshot{ID: "job", SessionID: s.Grant().SessionID, TaskID: "task-1", RequestID: "req", Binding: binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1}
	for _, mode := range []string{"task", "session", "binding", "hash", "state", "length", "large", "redirect"} {
		c.http.Transport = runtimeTestTransport(func(*http.Request) (*http.Response, error) {
			v := snap
			switch mode {
			case "task":
				v.TaskID = "other"
			case "session":
				v.SessionID = "other"
			case "binding":
				v.Binding.Revision++
			case "hash":
				v.BodySHA256 = strings.Repeat("0", 64)
			case "state":
				v.State = "new_success"
			}
			r := runtimeTestJSON(202, v)
			switch mode {
			case "length":
				r.ContentLength++
			case "large":
				r.ContentLength = runtimeBodyLimit + 1
			case "redirect":
				r.StatusCode = 307
				r.Header.Set("Location", "http://127.0.0.1:9999/")
			}
			return r, nil
		})
		if _, e := c.Submit(context.Background(), s, "req", binding, body); e == nil {
			t.Fatal("accepted", mode)
		}
	}
}
func runtimeTestWAV() []byte {
	b := make([]byte, 44+16000)
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
	binary.LittleEndian.PutUint32(b[40:], 16000)
	return b
}
func TestRuntimeClientWAVValidation(t *testing.T) {
	b := runtimeTestWAV()
	sum := sha256.Sum256(b)
	m := RuntimeMediaResult{ID: "media", Bytes: "16044", SHA256: hex.EncodeToString(sum[:]), SampleRate: 8000, Channels: 1, DurationMS: 1000}
	if e := runtimeWAV(bytes.NewReader(b), int64(len(b)), m); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"riff", "format", "rate", "duration", "data", "truncate"} {
		bad := append([]byte(nil), b...)
		meta := m
		switch mode {
		case "riff":
			bad[0] = 'X'
		case "format":
			bad[20] = 3
		case "rate":
			meta.SampleRate = 16000
		case "duration":
			meta.DurationMS = 2000
		case "data":
			bad[40] = 255
		case "truncate":
			bad = bad[:len(bad)-1]
		}
		if runtimeWAV(bytes.NewReader(bad), int64(len(b)), meta) == nil {
			t.Fatal(mode)
		}
	}
}
