package highlightclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

// These are transport contract tests, not model inference tests.
type fakeRuntime struct {
	submitErr, uploadErr, eventsErr, queryErr, resultErr, cancelErr error
	state                                                           integration.RuntimeState
	code                                                            string
	events                                                          []integration.RuntimeEvent
	result                                                          integration.RuntimeResult
	body                                                            integration.RuntimeBody
	ids                                                             []string
	calls                                                           []string
	uploaded                                                        []byte
	mime                                                            string
	corrupt                                                         func(*integration.RuntimeAsset)
	onEvents                                                        func(context.Context) error
	onAck                                                           func() error
	cancelContextOK                                                 bool
}

func (f *fakeRuntime) Upload(ctx context.Context, typ string, size int64, r io.Reader) (integration.RuntimeAsset, error) {
	f.calls = append(f.calls, "upload")
	f.mime = typ
	if f.uploadErr != nil {
		return integration.RuntimeAsset{}, f.uploadErr
	}
	b, e := io.ReadAll(r)
	if e != nil {
		return integration.RuntimeAsset{}, e
	}
	f.uploaded = b
	sum := sha256.Sum256(b)
	m := integration.RuntimeAssetMetadata{Kind: "image", Bytes: itoa(int64(len(b))), SHA256: hex.EncodeToString(sum[:]), Width: 2, Height: 2}
	if typ == "audio/wav" {
		m = integration.RuntimeAssetMetadata{Kind: "wav", Bytes: itoa(int64(len(b))), SHA256: hex.EncodeToString(sum[:]), SampleRate: 16000, Channels: 1, DurationSeconds: 1}
	}
	a := integration.RuntimeAsset{ID: "asset", Metadata: m}
	if f.corrupt != nil {
		f.corrupt(&a)
	}
	return a, nil
}
func (f *fakeRuntime) Submit(ctx context.Context, id string, b Binding, body integration.RuntimeBody) (*job, error) {
	f.calls = append(f.calls, "submit")
	f.ids = append(f.ids, id)
	f.body = body
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	raw, _ := json.Marshal(struct {
		Binding integration.ExecutionBindingRef
		Body    integration.RuntimeBody
	}{b.Binding, body})
	sum := sha256.Sum256(raw)
	return &job{snapshot: integration.RuntimeSnapshot{ID: "job", SessionID: "session", TaskID: "task", RequestID: id, Binding: b.Binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1}}, nil
}
func (f *fakeRuntime) Events(ctx context.Context, j *job, cursor uint64, emit func(integration.RuntimeEvent) error) error {
	f.calls = append(f.calls, "events")
	if cursor != 0 {
		return errors.New("cursor not zero")
	}
	if f.onEvents != nil {
		return f.onEvents(ctx)
	}
	for _, e := range f.events {
		if err := emit(e); err != nil {
			return err
		}
	}
	return f.eventsErr
}
func (f *fakeRuntime) Query(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	f.calls = append(f.calls, "query")
	s := j.snapshot
	s.State = f.state
	s.Code = f.code
	s.FinishedAt = s.CreatedAt.Add(time.Second)
	s.Cursor = 4
	return s, f.queryErr
}
func (f *fakeRuntime) Result(ctx context.Context, j *job) (integration.RuntimeResult, error) {
	f.calls = append(f.calls, "result")
	return f.result, f.resultErr
}
func (f *fakeRuntime) Cancel(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	f.calls = append(f.calls, "cancel")
	d, ok := ctx.Deadline()
	f.cancelContextOK = ctx.Err() == nil && ok && time.Until(d) > 0 && time.Until(d) <= 5*time.Second
	s := j.snapshot
	s.State = "cancelled"
	return s, f.cancelErr
}

func (f *fakeRuntime) Acknowledge(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	f.calls = append(f.calls, "ack")
	if f.onAck != nil {
		if e := f.onAck(); e != nil {
			return integration.RuntimeSnapshot{}, e
		}
	}
	s := j.snapshot
	s.State = "succeeded"
	s.FinishedAt = s.CreatedAt.Add(time.Second)
	s.Cursor = 4
	s.Acknowledged = true
	return s, nil
}

func testBinding(scene, component string) Binding {
	return Binding{Binding: integration.ExecutionBindingRef{Scene: scene, Revision: 1, SHA256: strings.Repeat("a", 64)}, Component: integration.ExecutionComponentRef{ID: component, Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "test.v1"}}
}
func setup(t *testing.T, scene, component string) (*Client, *fakeRuntime, *[]integration.RuntimeSnapshot) {
	t.Helper()
	b := testBinding(scene, component)
	f := &fakeRuntime{state: "succeeded", result: integration.RuntimeResult{Component: b.Component, Events: []*integration.RuntimeRawEvent{}}}
	records := []integration.RuntimeSnapshot{}
	g := integration.SessionGrant{SessionID: "session", TaskID: "task", Permission: integration.PermissionRuntime, Capabilities: []string{scene}, Bindings: []integration.ExecutionBindingRef{b.Binding}}
	c, e := newClient(f, func() integration.SessionGrant { return g }, []Binding{b}, func(s integration.RuntimeSnapshot) error { records = append(records, s); return nil }, func(r integration.RuntimeCompletion) error {
		raw, e := json.Marshal(r)
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(t.TempDir(), "result.json"), raw, 0600)
	})
	if e != nil {
		t.Fatal(e)
	}
	return c, f, &records
}
func candidate() scoring.CandidateInput {
	return scoring.CandidateInput{EpisodeNumber: 1, CandidateIndex: 2, StartSec: 0, EndSec: 1, MinHighlightScore: .93, MinCompletenessScore: .97}
}
func prepared(t *testing.T) (domain.Episode, audio.PreparedAudio) {
	t.Helper()
	b := make([]byte, 44+32000)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 32000)
	dir := t.TempDir()
	path := filepath.Join(dir, "track.wav")
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(b)
	ep := domain.Episode{EpisodeNumber: 1, VideoPath: filepath.Join(dir, "source.mp4")}
	return ep, audio.PreparedAudio{SourceVideoPath: ep.VideoPath, WAVPath: path, EpisodeNumber: 1, DurationSec: 1, MediaDurationSec: 1, SampleCount: 16000, Format: audio.AnalysisFormat(), ContentSHA256: hex.EncodeToString(sum[:])}
}
func imageInput(t *testing.T) *scoring.ImageInput {
	t.Helper()
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); e != nil {
		t.Fatal(e)
	}
	return &scoring.ImageInput{MIMEType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(b.Bytes())}
}
func commits() []integration.RuntimeEvent {
	return []integration.RuntimeEvent{{Sequence: 1, Commit: &integration.RuntimeStreamCommit{EpisodeNumber: 1, Revision: 1, CommittedThroughSec: .5, Items: []integration.RuntimeTranscriptItem{{StartSec: 0, EndSec: .5, Text: "first"}}}}, {Sequence: 2, Commit: &integration.RuntimeStreamCommit{EpisodeNumber: 1, Revision: 2, CommittedThroughSec: 1, Items: []integration.RuntimeTranscriptItem{{StartSec: .5, EndSec: 1, Text: "last"}}, Final: true}}}
}

func cumulativeStream(events []integration.RuntimeEvent) *integration.RuntimeStreamCommit {
	last := *events[len(events)-1].Commit
	last.Items = nil
	for _, event := range events {
		for _, item := range event.Commit.Items {
			item.Words = slices.Clone(item.Words)
			last.Items = append(last.Items, item)
		}
	}
	return &last
}

func TestNewRejectsMissingSessionAndInvalidScope(t *testing.T) {
	if _, e := New(nil, nil, nil, nil, nil); e == nil {
		t.Fatal("accepted missing runtime/session")
	}
	if _, e := New(integration.NewRuntimeClient(nil), &integration.SessionCredentials{}, []Binding{testBinding("highlight.score", "llama-runtime")}, func(integration.RuntimeSnapshot) error { return nil }, nil); e == nil {
		t.Fatal("accepted empty credentials")
	}
	b := testBinding("highlight.score", "llama-runtime")
	f := &fakeRuntime{}
	for _, g := range []integration.SessionGrant{{}, {Permission: integration.PermissionManagement}, {Permission: integration.PermissionRuntime, TaskID: "task", Capabilities: []string{"highlight.score"}, Bindings: []integration.ExecutionBindingRef{testBinding("highlight.vision", "llama-runtime").Binding}}} {
		if _, e := newClient(f, func() integration.SessionGrant { return g }, []Binding{b}, func(integration.RuntimeSnapshot) error { return nil }, nil); e == nil {
			t.Fatal("accepted unauthorized binding")
		}
	}
}
func TestCompletionRequiredAndFrozenSessionScope(t *testing.T) {
	for _, mode := range []string{"recorder", "task", "session", "instance"} {
		c, f, _ := setup(t, "highlight.score", "llama-runtime")
		if mode == "recorder" {
			if _, e := newClient(f, c.grant, []Binding{testBinding("highlight.score", "llama-runtime")}, c.record, nil); e == nil {
				t.Fatal("optional completion recorder")
			}
			continue
		}
		g := c.grant()
		switch mode {
		case "task":
			g.TaskID = "other"
		case "session":
			g.SessionID = "other"
		case "instance":
			g.InstanceID = "other"
		}
		c.grant = func() integration.SessionGrant { return g }
		if _, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), candidate()); e == nil || len(f.calls) != 0 {
			t.Fatal("scope changed", mode, e)
		}
	}
}

func TestScoreProjectionReceiptsAndRandomIDs(t *testing.T) {
	c, f, records := setup(t, "highlight.score", "llama-runtime")
	in := candidate()
	in.Transcript = "private local transcript"
	in.Context = "private context"
	f.result.Events = []*integration.RuntimeRawEvent{{HighlightScore: .12, CompletenessScore: .23, Title: "real transport result", Reason: "reason", Tags: []string{"tag"}, IncludeInCompilation: false}}
	for i := 0; i < 2; i++ {
		out, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), in)
		if e != nil {
			t.Fatal(e)
		}
		if len(out) != 1 || out[0].HighlightScore != .12 || out[0].IncludeInCompilation {
			t.Fatalf("modified raw event: %+v", out)
		}
	}
	if f.body.Score == nil || f.body.Vision != nil || f.body.Score.MinHighlightScore != .93 || f.body.Score.MinCompletenessScore != .97 || f.body.Score.TaskID != "task" {
		t.Fatalf("bad projection: %+v", f.body)
	}
	raw, _ := json.Marshal(f.body)
	for _, bad := range []string{"private", "path", "image", "data_base64"} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Fatalf("local data leaked: %s", raw)
		}
	}
	if f.ids[0] == f.ids[1] || len(f.ids[0]) < 32 {
		t.Fatal("request IDs not independently random")
	}
	if len(*records) != 6 {
		t.Fatalf("snapshots not recorded: %d", len(*records))
	}
	if !reflect.DeepEqual(f.calls, []string{"submit", "events", "query", "result", "ack", "submit", "events", "query", "result", "ack"}) {
		t.Fatal(f.calls)
	}
}
func TestCompleteResultDurableBeforeAck(t *testing.T) {
	for _, mode := range []string{"success", "durable-fails", "unknown-ack"} {
		t.Run(mode, func(t *testing.T) {
			c, f, records := setup(t, "highlight.score", "llama-runtime")
			durable := false
			sentinel := errors.New("offline store failure")
			path := filepath.Join(t.TempDir(), "receipt.json")
			c.complete = func(r integration.RuntimeCompletion) error {
				if mode == "durable-fails" {
					return sentinel
				}
				raw, e := json.Marshal(r)
				if e != nil {
					return e
				}
				if e = os.WriteFile(path, raw, 0600); e != nil {
					return e
				}
				durable = true
				r.Result.Events = append(r.Result.Events, &integration.RuntimeRawEvent{})
				return nil
			}
			f.onAck = func() error {
				if !durable {
					t.Fatal("Ack before durable")
				}
				if mode == "unknown-ack" {
					return sentinel
				}
				return nil
			}
			_, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), candidate())
			if (e == nil) != (mode == "success") {
				t.Fatal(e)
			}
			if len(f.ids) != 1 {
				t.Fatal("model replay")
			}
			if mode == "durable-fails" && slices.Contains(f.calls, "ack") {
				t.Fatal("Ack despite failed persistence")
			}
			if ((*records)[len(*records)-1].Acknowledged) != (mode == "success") {
				t.Fatal("false Ack persistence")
			}
		})
	}
}

func TestStreamCompletionOwnsEveryOrderedDelta(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
	ep, p := prepared(t)
	f.events = commits()
	f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
	var receipt integration.RuntimeCompletion
	c.complete = func(r integration.RuntimeCompletion) error {
		receipt = r
		if len(r.Commits) != 2 || r.Commits[0].Items[0].Text != "first" || r.Commits[1].Items[0].Text != "last" {
			t.Fatal("lost full stream")
		}
		r.Commits[0].Items[0].Text = "recorder mutation"
		r.Result.Stream.Items[0].Text = "recorder mutation"
		return nil
	}
	if e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(v transcript.StreamCommit) error { v.Items[0].Text = "consumer mutation"; return nil }); e != nil {
		t.Fatal(e)
	}
	if len(receipt.Commits) != 2 || f.events[0].Commit.Items[0].Text != "first" || f.result.Stream.Items[0].Text != "first" {
		t.Fatal("unowned stream data")
	}
}

func TestStreamReceiptAggregateBoundsRejectBeforeAck(t *testing.T) {
	for _, mode := range []string{"bytes", "count"} {
		t.Run(mode, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
			ep, p := prepared(t)
			if mode == "bytes" {
				f.events = commits()
				f.events[0].Commit.Items[0].Text = strings.Repeat("x", (4<<20)+1)
			} else {
				for i := 1; i <= 16385; i++ {
					f.events = append(f.events, integration.RuntimeEvent{Sequence: uint64(i), Commit: &integration.RuntimeStreamCommit{EpisodeNumber: 1, Revision: uint64(i), CommittedThroughSec: .5}})
				}
			}
			if e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { return nil }); e == nil {
				t.Fatal("accepted oversized stream receipt")
			}
			if slices.Contains(f.calls, "ack") || len(f.ids) != 1 {
				t.Fatal("Ack/replay on bounded failure")
			}
		})
	}
}

func TestTextNeverSilentlyDropsVisualInput(t *testing.T) {
	for _, path := range []bool{false, true} {
		c, f, _ := setup(t, "highlight.score", "llama-runtime")
		in := candidate()
		if path {
			in.ContactSheetPath = "private.png"
		} else {
			in.Image = imageInput(t)
		}
		if _, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), in); e == nil {
			t.Fatal("ignored visual input")
		}
		if len(f.calls) != 0 {
			t.Fatal(f.calls)
		}
	}
}
func TestVisionUploadsActualBytes(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(itoa(int64(boolInt(local))), func(t *testing.T) {
			c, f, _ := setup(t, "highlight.vision", "llama-runtime")
			in := candidate()
			img := imageInput(t)
			want, _ := base64.StdEncoding.DecodeString(img.DataBase64)
			if local {
				p := filepath.Join(t.TempDir(), "sheet.png")
				if e := os.WriteFile(p, want, 0600); e != nil {
					t.Fatal(e)
				}
				in.ContactSheetPath = p
			} else {
				in.Image = img
			}
			if _, e := c.Scorer("highlight.vision").ScoreEvents(context.Background(), in); e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(f.uploaded, want) || f.mime != "image/png" || f.body.Vision == nil || f.body.Vision.AssetID != "asset" || f.body.Score != nil {
				t.Fatal("vision was not uploaded/projected")
			}
			raw, _ := json.Marshal(f.body)
			if strings.Contains(string(raw), "path") || strings.Contains(string(raw), "base64") {
				t.Fatal(string(raw))
			}
		})
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestVisionRejectsMissingCorruptMIMEAndOversize(t *testing.T) {
	for _, mutate := range []func(*scoring.CandidateInput){func(i *scoring.CandidateInput) {}, func(i *scoring.CandidateInput) {
		i.Image = &scoring.ImageInput{MIMEType: "image/gif", DataBase64: "AAAA"}
	}, func(i *scoring.CandidateInput) { i.Image = imageInput(t); i.Image.MIMEType = "image/jpeg" }, func(i *scoring.CandidateInput) {
		i.Image = &scoring.ImageInput{MIMEType: "image/png", DataBase64: "invalid"}
	}, func(i *scoring.CandidateInput) {
		i.Image = &scoring.ImageInput{MIMEType: "image/png", DataBase64: strings.Repeat("A", 12<<20)}
	}, func(i *scoring.CandidateInput) { i.Image = imageInput(t); i.ContactSheetPath = "another.png" }} {
		c, f, _ := setup(t, "highlight.vision", "llama-runtime")
		in := candidate()
		mutate(&in)
		if _, e := c.Scorer("highlight.vision").ScoreEvents(context.Background(), in); e == nil {
			t.Fatal("accepted bad image")
		}
		if len(f.calls) != 0 {
			t.Fatal(f.calls)
		}
	}
}
func TestOperationFailuresDoNotReplayOrBlindCancel(t *testing.T) {
	sentinel := errors.New("transport test failure")
	for _, stage := range []string{"submit", "events", "query", "result", "failed", "running", "410", "record"} {
		t.Run(stage, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.score", "llama-runtime")
			switch stage {
			case "submit":
				f.submitErr = sentinel
			case "events":
				f.eventsErr = sentinel
			case "query":
				f.queryErr = sentinel
			case "result":
				f.resultErr = sentinel
			case "failed", "running":
				f.state = integration.RuntimeState(stage)
			case "410":
				f.eventsErr = integration.ErrRuntimeEventCursorExpired
			case "record":
				c.record = func(integration.RuntimeSnapshot) error { return sentinel }
			}
			_, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), candidate())
			if e == nil {
				t.Fatal("failure became success")
			}
			if len(f.ids) != 1 {
				t.Fatal("submit replayed")
			}
			for _, call := range f.calls {
				if call == "cancel" {
					t.Fatal("blind cancellation")
				}
			}
			if stage != "failed" && stage != "running" && stage != "410" && !errors.Is(e, sentinel) {
				t.Fatalf("lost error: %v", e)
			}
		})
	}
}
func TestTerminalFailureCodesThroughScorer(t *testing.T) {
	cases := []struct{ name, code, want string }{
		{"model", "center_model_incompatible", "center_model_incompatible"},
		{"binding", "center_binding_stale", "center_binding_stale"},
		{"component", "center_component_missing", "center_component_missing"},
		{"protocol", "center_protocol_unsupported", "center_protocol_unsupported"},
		{"whisper", "center_whisper_failed", "center_whisper_failed"},
		{"llama-load", "center_llama_load_failed", "center_llama_load_failed"},
		{"llama-inference", "center_llama_inference_failed", "center_llama_inference_failed"},
		{"result", "center_result_invalid", "center_result_invalid"},
		{"request", "center_request_invalid", "center_request_invalid"},
		{"busy", "center_runtime_busy", "center_runtime_busy"},
		{"executor", "center_executor_failed", "center_executor_failed"},
		{"job-timeout", "center_job_timeout", "center_job_timeout"},
		{"queue-timeout", "center_queue_timeout", "center_queue_timeout"},
		{"job-cancelled", "center_job_cancelled", "center_job_cancelled"},
		{"session-expired", "center_session_expired", "center_session_expired"},
		{"closed", "center_closed", "center_closed"},
		{"empty", "", "center_executor_failed"},
		{"unknown", "center_future_failure", "center_executor_failed"},
		{"unlisted", "center_session_denied", "center_executor_failed"},
		{"guessed-cancel", "center_cancelled", "center_executor_failed"},
		{"guessed-timeout", "center_timeout", "center_executor_failed"},
		{"uppercase", "CENTER_LLAMA_LOAD_FAILED", "center_executor_failed"},
		{"whitespace", " center_llama_load_failed\n", "center_executor_failed"},
		{"url", "center_llama_load_failed https://private.invalid/model?token=secret-token", "center_executor_failed"},
		{"token", "Bearer secret-token", "center_executor_failed"},
		{"windows-path", `C:\private\models\secret.gguf`, "center_executor_failed"},
		{"unix-path", "/private/models/secret.gguf", "center_executor_failed"},
		{"model-text", "center_llama_inference_failed: private model dialogue", "center_executor_failed"},
	}
	for _, state := range []integration.RuntimeState{"failed", "cancelled"} {
		for _, tc := range cases {
			t.Run(string(state)+"/"+tc.name, func(t *testing.T) {
				c, f, records := setup(t, "highlight.score", "llama-runtime")
				f.state, f.code = state, tc.code
				f.eventsErr = io.EOF
				c.complete = func(integration.RuntimeCompletion) error {
					t.Fatal("persisted a completion for a failed job")
					return nil
				}
				out, err := c.Scorer("highlight.score").ScoreEvents(context.Background(), candidate())
				if err == nil || out != nil {
					t.Fatalf("terminal failure returned success: %v", err)
				}
				if want := "center_job_" + string(state); err.Error() != want {
					t.Fatalf("error text = %q, want %q", err.Error(), want)
				}
				var diagnostic interface{ FailureCode() string }
				if !errors.As(fmt.Errorf("scorer: %w", err), &diagnostic) || diagnostic.FailureCode() != tc.want {
					t.Fatalf("failure code not preserved: %#v", err)
				}
				if errors.Unwrap(err) != nil {
					t.Fatal("terminal error retained a cause")
				}
				raw, e := json.Marshal(struct {
					Err  error
					Code string
				}{err, diagnostic.FailureCode()})
				if e != nil {
					t.Fatal(e)
				}
				if tc.code != "" && tc.code != tc.want {
					for _, formatted := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%s", err), fmt.Sprintf("%q", err), string(raw)} {
						for _, secret := range []string{tc.code, "private", "secret-token", "secret.gguf", "dialogue"} {
							if strings.Contains(formatted, secret) {
								t.Fatalf("unsafe diagnostic retained: %s", formatted)
							}
						}
					}
				}
				if len(f.ids) != 1 || !reflect.DeepEqual(f.calls, []string{"submit", "events", "query"}) {
					t.Fatalf("failure retried, fetched, acknowledged or cancelled: %v", f.calls)
				}
				if len(*records) != 2 {
					t.Fatalf("snapshot records = %d, want 2", len(*records))
				}
				want := (*records)[0]
				want.State, want.Code = state, tc.code
				want.FinishedAt = want.CreatedAt.Add(time.Second)
				want.Cursor = 4
				if !reflect.DeepEqual((*records)[1], want) {
					t.Fatal("original query snapshot was not persisted unchanged")
				}
			})
		}
	}
}

func TestBatchPreparedValidationAndIdentity(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
	ep, p := prepared(t)
	f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-batch").Component, Items: []integration.RuntimeTranscriptItem{{StartSec: 0, EndSec: 1, Text: "spoken", Words: []integration.RuntimeTranscriptWord{{StartSec: 0, EndSec: .5, Text: "spoken"}}}}}
	b := c.Transcriber(true)
	var _ transcript.Transcriber = b
	var _ transcript.PreparedAudioTranscriber = b
	out, e := b.TranscribeWithAudio(context.Background(), ep, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	if out.Source != b.Identity() || out.Source == "" || out.ConfirmedNoSpeech || len(out.Items) != 1 || len(out.Items[0].Words) != 1 || f.body.Transcribe == nil || !f.body.Transcribe.VAD || f.body.Transcribe.Stream {
		t.Fatalf("bad batch output: %+v", out)
	}
	before := len(f.calls)
	if _, e = b.Transcribe(context.Background(), ep, nil); e == nil || len(f.calls) != before {
		t.Fatal("unprepared batch must explicitly fail")
	}
}
func TestPreparedRejectsIdentityAndUploadDifferences(t *testing.T) {
	for _, kind := range []string{"episode", "source", "sha", "samples", "format", "duration", "upload-sha", "upload-format", "upload-duration", "upload-bytes", "upload-kind"} {
		t.Run(kind, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
			ep, p := prepared(t)
			switch kind {
			case "episode":
				p.EpisodeNumber++
			case "source":
				p.SourceVideoPath += "other"
			case "sha":
				p.ContentSHA256 = strings.Repeat("0", 64)
			case "samples":
				p.SampleCount--
			case "format":
				p.Format.Channels = 2
			case "duration":
				p.DurationSec = 2
			default:
				f.corrupt = func(a *integration.RuntimeAsset) {
					switch kind {
					case "upload-sha":
						a.Metadata.SHA256 = strings.Repeat("0", 64)
					case "upload-format":
						a.Metadata.SampleRate = 8000
					case "upload-duration":
						a.Metadata.DurationSeconds = .9
					case "upload-bytes":
						a.Metadata.Bytes = "1"
					case "upload-kind":
						a.Metadata.Kind = "image"
					}
				}
			}
			if _, e := c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, nil); e == nil {
				t.Fatal("accepted prepared mismatch")
			}
			if len(f.ids) != 0 {
				t.Fatal("submitted invalid audio")
			}
		})
	}
}
func TestBatchEmptyIsNotNoSpeech(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
	ep, p := prepared(t)
	f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-batch").Component, Items: []integration.RuntimeTranscriptItem{}}
	out, e := c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, nil)
	if e == nil || out.ConfirmedNoSpeech {
		t.Fatal("empty ASR forged no speech")
	}
}
func TestStreamMapsEveryCommitAndChecksCumulativeResult(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
	ep, p := prepared(t)
	f.events = commits()
	f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
	var got []transcript.StreamCommit
	e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(v transcript.StreamCommit) error {
		got = append(got, v)
		v.Items[0].Text = "caller mutation"
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 2 || got[0].Revision != 1 || !got[1].Final || got[1].Revision != 2 || !f.body.Transcribe.Stream {
		t.Fatalf("bad commits: %+v", got)
	}
}
func TestStreamRejectsLostDuplicateReorderedAndMismatchedFinal(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		emitted int
		result  bool
	}{
		{"missing", 0, true}, {"410", 0, false}, {"duplicate", 1, false},
		{"reordered", 0, false}, {"sequence-gap", 1, false}, {"after-final", 2, false},
		{"wrong-episode", 0, false}, {"missing-final", 1, true},
		{"result-mismatch", 2, true}, {"result-episode", 2, true}, {"result-through", 2, true},
		{"result-not-final", 2, true}, {"result-delta-only", 2, true},
		{"result-early-tamper", 2, true}, {"result-missing-last", 2, true}, {"result-empty", 2, true},
		{"end-short", 1, false}, {"overlap", 1, false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
			ep, p := prepared(t)
			f.events = commits()
			f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
			completed := false
			c.complete = func(integration.RuntimeCompletion) error { completed = true; return nil }
			switch tc.kind {
			case "missing":
				f.events = nil
			case "410":
				f.events = nil
				f.eventsErr = integration.ErrRuntimeEventCursorExpired
			case "duplicate":
				f.events = append([]integration.RuntimeEvent{f.events[0]}, f.events...)
			case "reordered":
				f.events[0], f.events[1] = f.events[1], f.events[0]
			case "sequence-gap":
				f.events[1].Sequence++
			case "after-final":
				extra := *f.events[1].Commit
				extra.Revision++
				extra.Items = nil
				f.events = append(f.events, integration.RuntimeEvent{Sequence: 3, Commit: &extra})
			case "wrong-episode":
				f.events[0].Commit.EpisodeNumber = 2
			case "missing-final":
				f.events = f.events[:1]
			case "result-mismatch":
				f.result.Stream.Revision++
			case "result-episode":
				f.result.Stream.EpisodeNumber++
			case "result-through":
				f.result.Stream.CommittedThroughSec += .1
			case "result-not-final":
				f.result.Stream.Final = false
			case "result-delta-only":
				f.result.Stream.Items = slices.Clone(f.events[1].Commit.Items)
			case "result-early-tamper":
				f.result.Stream.Items[0].Text = "tampered"
			case "result-missing-last":
				f.result.Stream.Items = f.result.Stream.Items[:1]
			case "result-empty":
				f.result.Stream.Items = nil
			case "end-short":
				f.events[1].Commit.CommittedThroughSec = .9
				f.events[1].Commit.Items[0].EndSec = .9
				f.result.Stream = cumulativeStream(f.events)
			case "overlap":
				f.events[1].Commit.Items[0].StartSec = .4
				f.result.Stream = cumulativeStream(f.events)
			}
			emitted := 0
			e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { emitted++; return nil })
			if e == nil {
				t.Fatal("stream corruption became success")
			}
			if emitted != tc.emitted || slices.Contains(f.calls, "result") != tc.result {
				t.Fatalf("wrong gate: emitted=%d calls=%v error=%v", emitted, f.calls, e)
			}
			if tc.kind == "410" && !errors.Is(e, integration.ErrRuntimeEventCursorExpired) || tc.kind != "410" && !errors.Is(e, errResult) {
				t.Fatalf("wrong error: %v", e)
			}
			if completed || slices.Contains(f.calls, "ack") || slices.Contains(f.calls, "cancel") {
				t.Fatalf("persisted, acknowledged or cancelled corrupt stream: %v", f.calls)
			}
		})
	}
}
func TestStreamEmptyFinalKeepsCumulativeItemsAndNoSpeechSemantics(t *testing.T) {
	for _, noSpeech := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-speech=%t", noSpeech), func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
			ep, p := prepared(t)
			f.events = commits()
			f.events[1].Commit.Items = nil
			if noSpeech {
				f.events[0].Commit.Items = nil
			}
			f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
			emitted := 0
			e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(v transcript.StreamCommit) error {
				emitted++
				if v.Final && len(v.Items) != 0 {
					t.Fatal("empty final delta replaced with cumulative items")
				}
				return nil
			})
			if (e != nil) != noSpeech || emitted != 2 || slices.Contains(f.calls, "ack") == noSpeech {
				t.Fatalf("no-speech=%t error=%v emitted=%d calls=%v", noSpeech, e, emitted, f.calls)
			}
		})
	}
}

func TestStreamOptionsNeverIgnored(t *testing.T) {
	for _, o := range []transcript.StreamOptions{{}, transcript.DefaultStreamOptions(), {TimestampToleranceSec: .2, FinalizeDelaySec: 2}, {WindowSec: 40, StepSec: 1, StableRounds: 3, TimestampToleranceSec: .4, FinalizeDelaySec: 4}, {WindowSec: 30, StepSec: 2, StableRounds: 2}, {WindowSec: -1}, {StepSec: 11}, {StableRounds: 6}, {TimestampToleranceSec: math.NaN()}, {FinalizeDelaySec: math.Inf(1)}} {
		c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
		ep, p := prepared(t)
		f.events = commits()
		f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
		e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, o, func(transcript.StreamCommit) error { return nil })
		want := o.WithDefaults()
		if o == (transcript.StreamOptions{}) {
			want = transcript.DefaultStreamOptions()
		}
		supported := want.Validate() == nil
		if supported && e != nil || !supported && e == nil {
			t.Fatalf("options %+v: %v", o, e)
		}
		if !supported && len(f.calls) > 0 {
			t.Fatal("invalid options started work")
		}
		if supported {
			raw, _ := json.Marshal(f.body.Transcribe)
			var wire struct {
				Options *transcript.StreamOptions `json:"options"`
			}
			if json.Unmarshal(raw, &wire) != nil || wire.Options == nil || *wire.Options != want {
				t.Fatalf("options lost: %s, want %+v", raw, want)
			}
		}
	}
}
func TestCancellationAndEmitErrorsCancelOnlyOwnedJob(t *testing.T) {
	sentinel := errors.New("emit stopped")
	for _, kind := range []string{"context", "emit", "cancel-fails"} {
		t.Run(kind, func(t *testing.T) {
			c, f, records := setup(t, "highlight.transcribe", "whisper-stream")
			ep, p := prepared(t)
			f.events = commits()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "context" {
				f.onEvents = func(context.Context) error { cancel(); return ctx.Err() }
			}
			if kind == "cancel-fails" {
				f.cancelErr = errors.New("cancel failed")
			}
			e := c.StreamingTranscriber(false).TranscribeStream(ctx, ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { return sentinel })
			want := sentinel
			if kind == "context" {
				want = context.Canceled
			}
			if !errors.Is(e, want) {
				t.Fatalf("lost cancellation cause: %v", e)
			}
			if !f.cancelContextOK {
				t.Fatal("Cancel reused cancelled/unbounded context")
			}
			if f.calls[len(f.calls)-1] != "cancel" {
				t.Fatal(f.calls)
			}
			if kind != "cancel-fails" && (*records)[len(*records)-1].State != "cancelled" {
				t.Fatal("cancel receipt not saved")
			}
			if kind == "cancel-fails" && !errors.Is(e, f.cancelErr) {
				t.Fatal("lost cancel error")
			}
		})
	}
}
