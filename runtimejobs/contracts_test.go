package runtimejobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func streamBody() Body {
	b := body("highlight.score")
	b.Score = nil
	b.Component.ID = "whisper-stream"
	b.Transcribe = &TranscribeInput{AssetID: "audio", Stream: true, EpisodeNumber: 1, OffsetMS: 1000}
	return b
}
func TestStreamOptionsAdmissionAndFrozenHash(t *testing.T) {
	b := streamBody()
	legacy, _ := json.Marshal(b)
	options := `{"window_sec":40,"step_sec":1,"stable_rounds":3,"timestamp_tolerance_sec":0,"finalize_delay_sec":0}`
	withOptions := func(o string) []byte {
		return []byte(strings.TrimSuffix(string(legacy), "}}") + `,"options":` + o + `}}`)
	}
	custom, err := DecodeBody(withOptions(options))
	if err != nil || !custom.validate("highlight.transcribe") {
		t.Fatalf("custom options rejected: %v", err)
	}
	for _, bad := range []string{`null`, `{}`, strings.Replace(options, `40`, `0`, 1), strings.Replace(options, `"step_sec":1`, `"step_sec":0`, 1), strings.Replace(options, `"stable_rounds":3`, `"stable_rounds":0`, 1), strings.Replace(options, `40`, `121`, 1), strings.Replace(options, `"window_sec":40`, `"unknown":40`, 1), strings.Replace(options, `40`, `null`, 1), strings.Replace(options, `40`, `NaN`, 1), strings.Replace(options, `40`, `1e999`, 1)} {
		v, err := DecodeBody(withOptions(bad))
		if err == nil && v.validate("highlight.transcribe") {
			t.Fatalf("accepted invalid options %s", bad)
		}
	}
	batch := custom
	input := *custom.Transcribe
	batch.Transcribe = &input
	batch.Transcribe.Stream = false
	batch.Component.ID = "whisper-batch"
	if batch.validate("highlight.transcribe") {
		t.Fatal("batch accepted stream options")
	}
	e := newControlled()
	m, _ := New(e, Options{})
	s, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), custom)
	if err != nil {
		t.Fatal(err)
	}
	call := <-e.calls
	defer func() {
		m.ExpireSession("session")
		call.reply <- Result{}
		m.Close()
	}()
	same, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), custom)
	if err != nil || same.ID != s.ID {
		t.Fatal("same options not idempotent", err)
	}
	changed, err := DecodeBody(withOptions(strings.Replace(options, `40`, `50`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), changed)
	code(t, err, 409)
	other, err := m.Submit(context.Background(), "session", "task", "different", binding("highlight.transcribe"), changed)
	if err != nil || other.BodySHA256 == s.BodySHA256 {
		t.Fatal("options absent from bodySHA", err)
	}
	old, err := m.Submit(context.Background(), "session", "task", "legacy", binding("highlight.transcribe"), b)
	if err != nil || old.BodySHA256 == s.BodySHA256 {
		t.Fatal("nil options hash lost", err)
	}
	got, _ := json.Marshal(call.request.Body)
	if string(got) != string(withOptions(options)) {
		t.Fatalf("executor lost options: %s", got)
	}
	got, _ = json.Marshal(b)
	if string(got) != string(legacy) {
		t.Fatal("legacy wire changed")
	}
}

func stableCommit() StreamCommit {
	p := 0.8
	return StreamCommit{EpisodeNumber: 1, Revision: 1, CommittedThroughSec: 3, Items: []TranscriptItem{{StartSec: 1, EndSec: 2, Text: "hello", Probability: &p, Words: []TranscriptWord{{StartSec: 1, EndSec: 2, Text: "hello", Probability: &p}}}}}
}
func TestStableCommitEvents(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), streamBody())
	if err != nil {
		t.Fatal(err)
	}
	call := <-e.calls
	defer func() { call.reply <- Result{}; m.Close() }()
	c := stableCommit()
	if err = call.emit(Progress{Stage: StageRunning, Commit: &c}); err != nil {
		t.Fatal(err)
	}
	c.Items[0].Words[0].Text = "mutation"
	events, _, _ := m.Events("session", "task", s.ID, s.Cursor)
	var saved *StreamCommit
	for _, v := range events {
		if v.Commit != nil {
			saved = v.Commit
		}
	}
	if saved == nil || saved.Items[0].Words[0].Text != "hello" {
		t.Fatal("commit not detached", events)
	}
	saved.Items[0].Words[0].Text = "reader mutation"
	again, _, _ := m.Events("session", "task", s.ID, s.Cursor)
	for _, v := range again {
		if v.Commit != nil && v.Commit.Items[0].Words[0].Text != "hello" {
			t.Fatal("reader alias")
		}
	}
	bad := stableCommit()
	bad.Revision = 2
	bad.Items[0].Text = "rewrite"
	if call.emit(Progress{Stage: StageRunning, Commit: &bad}) == nil {
		t.Fatal("committed prefix rewritten")
	}
	bad = stableCommit()
	bad.Revision = 2
	bad.Items = nil
	bad.CommittedThroughSec = 2
	if call.emit(Progress{Stage: StageRunning, Commit: &bad}) == nil {
		t.Fatal("watermark regressed")
	}
	next := StreamCommit{EpisodeNumber: 1, Revision: 2, CommittedThroughSec: 4, Items: []TranscriptItem{}, Final: true}
	if err = call.emit(Progress{Stage: StageRunning, Commit: &next}); err != nil {
		t.Fatal(err)
	}
	next.Revision++
	if call.emit(Progress{Stage: StageRunning, Commit: &next}) == nil {
		t.Fatal("post final commit")
	}
	m.Cancel("session", "task", s.ID)
	if call.emit(Progress{Stage: StageRunning, Commit: &next}) == nil {
		t.Fatal("post cancel commit")
	}
}
func TestCommitValidationAndFinalResult(t *testing.T) {
	for _, mutate := range []func(*StreamCommit){
		func(c *StreamCommit) { c.Revision = 0 }, func(c *StreamCommit) { c.EpisodeNumber = 2 },
		func(c *StreamCommit) { c.Items[0].StartSec = 0 }, func(c *StreamCommit) { c.CommittedThroughSec = 1.5 },
		func(c *StreamCommit) { c.Items[0].Words[0].EndSec = 2.5 }, func(c *StreamCommit) { *c.Items[0].Probability = 1.1 },
	} {
		e := newControlled()
		m, _ := New(e, Options{})
		s, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), streamBody())
		if err != nil {
			t.Fatal(err)
		}
		call := <-e.calls
		c := stableCommit()
		mutate(&c)
		if call.emit(Progress{Stage: StageRunning, Commit: &c}) == nil {
			t.Fatal("invalid commit accepted", c)
		}
		c = stableCommit()
		c.Final = true
		if err = call.emit(Progress{Stage: StageRunning, Commit: &c}); err != nil {
			t.Fatal(err)
		}
		call.reply <- Result{Component: call.request.Body.Component, Stream: &c}
		if waitTerminal(t, m, s.ID).State != Succeeded {
			t.Fatal("valid final rejected")
		}
		m.Close()
	}
}
func TestScoringContractAndSafeProjection(t *testing.T) {
	input := *body("highlight.score").Score
	input.EligibleUnits[0].Words = []TranscriptWord{}
	input.AnalysisChain = "audio_text_v1"
	input.AudioFeatures = &CandidateAudioFeatures{Schema: "audio_features_v1", Window: AudioInterval{StartSec: 0, EndSec: 1}, VAD: VADStatus{Backend: "whisper-vad", Reliable: true}, Speech: []AudioInterval{{StartSec: 0, EndSec: 1}}, Silence: []AudioInterval{}, Pauses: []AudioInterval{}, Overlaps: []OverlapInterval{}, UnitMetrics: []UnitAudioMetrics{}, SoundEvents: []SoundEvent{}, Loudness: LoudnessMetrics{Reason: "analysis_unavailable"}, SpeechRate: SpeechRateMetrics{Reason: "analysis_unavailable"}, Pitch: PitchMetrics{Reason: "analysis_unavailable"}}
	raw, _ := json.Marshal(input)
	projected, err := ScoreInputFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	projected.ContextUnits = []SpeechUnit{{ID: "context", StartSec: 0, EndSec: 1, Text: "context", Words: []TranscriptWord{}}}
	if !projected.valid() {
		t.Fatal("complete scoring input rejected")
	}
	b := body("highlight.score")
	b.Score = &projected
	raw, _ = json.Marshal(b)
	// Non-null word arrays are required on the strict wire.
	b.Score.EligibleUnits[0].Words = []TranscriptWord{}
	raw, _ = json.Marshal(b)
	if _, err = DecodeBody(raw); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"contact_sheet_path":"C:/private.jpg"}`, `{"url":"http://localhost"}`, `{"image":{"mime_type":"image/png","data_base64":"AAAA"}}`} {
		if _, err = ScoreInputFromJSON([]byte(raw)); err == nil {
			t.Fatal("unsafe projection", raw)
		}
	}
	input.AudioFeatures.Window.EndSec = 0.5
	if input.valid() {
		t.Fatal("out of range feature window")
	}
	input = *body("highlight.score").Score
	id := "c"
	bad := "context"
	start, end := 0.0, 1.0
	event := &RawEvent{HighlightScore: 0.9, ConflictScore: 0.8, EmotionScore: 0.7, ReversalScore: 0.6, VisualScore: 0.5, HookScore: 0.4, CompletenessScore: 1, StartUnitID: &id, EndUnitID: &id, RecommendedStartSec: &start, RecommendedEndSec: &end, Title: "title", Reason: "reason", Tags: []string{}, IncludeInCompilation: true}
	if !eventValid(event, input) {
		t.Fatal("complete event rejected")
	}
	event.EndUnitID = &bad
	if eventValid(event, input) {
		t.Fatal("context reference accepted")
	}
	event.EndUnitID = &id
	end = 2
	if eventValid(event, input) {
		t.Fatal("out of candidate recommendation")
	}
}

func TestMediaDecimalBytesValidation(t *testing.T) {
	b := body("highlight.score")
	b.Score = nil
	b.Component.ID = "seedvc-runner"
	b.Convert = &ConvertInput{AssetID: "audio", ReferenceAssetID: "ref"}
	req := Request{Binding: binding("timbre.convert"), Body: b}
	r := Result{Component: b.Component, Media: []MediaResult{{ID: "result", Bytes: "123", SHA256: binding("timbre.convert").SHA256, SampleRate: 44100, Channels: 2, DurationMS: 1000}}}
	if !r.validate(req) {
		t.Fatal("decimal bytes rejected")
	}
	for _, value := range []string{"0", "-1", "01", "1.0", "+1", "1073741825", "999999999999999999999"} {
		r.Media[0].Bytes = value
		if r.validate(req) {
			t.Fatal("invalid bytes", value)
		}
	}
}

func TestEmptyScoringEventsRemainSuccessfulEmptyArray(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	defer m.Close()
	s := submit(t, m, "empty")
	call := <-e.calls
	call.reply <- Result{Component: call.request.Body.Component, Events: []*RawEvent{}}
	if got := waitTerminal(t, m, s.ID); got.State != Succeeded {
		t.Fatal(got)
	}
	r, err := m.Result("session", "task", s.ID)
	if err != nil || r.Events == nil || len(r.Events) != 0 {
		t.Fatal("empty events lost", r, err)
	}
}

func TestStreamDeltasProduceCumulativeResult(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	defer m.Close()
	s, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), streamBody())
	if err != nil {
		t.Fatal(err)
	}
	call := <-e.calls
	c := stableCommit()
	data, _ := json.Marshal(c)
	projected, err := StreamCommitFromJSON(data, *call.request.Body.Transcribe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = call.emit(Progress{Stage: StageRunning, Commit: &projected}); err != nil {
		t.Fatal(err)
	}
	final := StreamCommit{EpisodeNumber: 1, Revision: 2, CommittedThroughSec: 4, Items: []TranscriptItem{}, Final: true}
	if err = call.emit(Progress{Stage: StageRunning, Commit: &final}); err != nil {
		t.Fatal(err)
	}
	final.Items = c.Items
	call.reply <- Result{Component: call.request.Body.Component, Stream: &final}
	if got := waitTerminal(t, m, s.ID); got.State != Succeeded {
		t.Fatal(got)
	}
}

func TestStreamFinalCannotRewritePrefix(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	defer m.Close()
	s, err := m.Submit(context.Background(), "session", "task", "stream", binding("highlight.transcribe"), streamBody())
	if err != nil {
		t.Fatal(err)
	}
	call := <-e.calls
	c := stableCommit()
	c.Final = true
	if err = call.emit(Progress{Stage: StageRunning, Commit: &c}); err != nil {
		t.Fatal(err)
	}
	c.Items[0].Words[0].Text = "changed"
	call.reply <- Result{Component: call.request.Body.Component, Stream: &c}
	if got := waitTerminal(t, m, s.ID); got.State != Failed || got.Code != "center_result_invalid" {
		t.Fatal(got)
	}
}

func TestRawEventNullableReferencesAndMediaBytes(t *testing.T) {
	var e RawEvent
	raw := `{"highlight_score":0.9,"conflict_score":0.8,"emotion_score":0.7,"reversal_score":0.6,"visual_score":0.5,"hook_score":0.4,"completeness_score":1,"start_unit_id":null,"end_unit_id":null,"recommended_start_sec":null,"recommended_end_sec":null,"title":"title","reason":"reason","tags":[],"include_in_compilation":false}`
	if err := decodeStrict([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"highlight_score":null}`, `{"tags":null}`, `{"start_unit_id":null,"start_unit_id":null}`} {
		if decodeStrict([]byte(raw), &e) == nil {
			t.Fatal("invalid raw event", raw)
		}
	}
	data, _ := json.Marshal(MediaResult{Bytes: "123"})
	var obj map[string]any
	json.Unmarshal(data, &obj)
	if obj["bytes"] != "123" {
		t.Fatal(string(data))
	}
}
