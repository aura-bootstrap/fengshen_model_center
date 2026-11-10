package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRuntimeCompletionStreamValidation(t *testing.T) {
	valid := func() RuntimeCompletion {
		binding := ExecutionBindingRef{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("a", 64)}
		component := ExecutionComponentRef{ID: "whisper-stream", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "stream.v1"}
		body := RuntimeBody{Component: component, Transcribe: &RuntimeTranscribeInput{AssetID: "audio", Stream: true, EpisodeNumber: 1}}
		frozen, err := json.Marshal(struct {
			Binding ExecutionBindingRef
			Body    RuntimeBody
		}{binding, body})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(frozen)
		created := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
		probability := .9
		commits := []RuntimeStreamCommit{
			{EpisodeNumber: 1, Revision: 3, CommittedThroughSec: .5, Items: []RuntimeTranscriptItem{{StartSec: 0, EndSec: .5, Text: "first", Probability: &probability, Words: []RuntimeTranscriptWord{{StartSec: 0, EndSec: .5, Text: "first", Probability: &probability}}}}},
			{EpisodeNumber: 1, Revision: 5, CommittedThroughSec: 1, Items: []RuntimeTranscriptItem{{StartSec: .5, EndSec: 1, Text: "last"}}, Final: true},
		}
		last := commits[1]
		last.Items = append(append([]RuntimeTranscriptItem(nil), commits[0].Items...), last.Items...)
		r := RuntimeCompletion{
			Snapshot: RuntimeSnapshot{ID: "job", SessionID: "session", TaskID: "task", RequestID: "request", Binding: binding, BodySHA256: hex.EncodeToString(sum[:]), State: "succeeded", CreatedAt: created, FinishedAt: created.Add(time.Second), Cursor: 8},
			Body:     body, Result: RuntimeResult{Component: component, Stream: &last}, Commits: commits,
		}
		// Give mutations independent item, word and probability storage.
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var owned RuntimeCompletion
		if err := json.Unmarshal(raw, &owned); err != nil {
			t.Fatal(err)
		}
		return owned
	}
	for _, tc := range []struct {
		name   string
		mutate func(*RuntimeCompletion)
	}{
		{"cumulative", func(*RuntimeCompletion) {}},
		{"empty_final_delta", func(r *RuntimeCompletion) {
			r.Commits[1].Items = []RuntimeTranscriptItem{}
			r.Result.Stream.Items = r.Result.Stream.Items[:1]
		}},
		{"no_speech_nil", func(r *RuntimeCompletion) {
			r.Commits[0].Items = nil
			r.Commits[1].Items = nil
			r.Result.Stream.Items = nil
		}},
		{"no_speech_empty", func(r *RuntimeCompletion) {
			r.Commits[0].Items = nil
			r.Commits[1].Items = nil
			r.Result.Stream.Items = []RuntimeTranscriptItem{}
		}},
		{"no_speech_empty_commits", func(r *RuntimeCompletion) {
			r.Commits[0].Items = []RuntimeTranscriptItem{}
			r.Commits[1].Items = []RuntimeTranscriptItem{}
			r.Result.Stream.Items = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("valid complete stream rejected: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*RuntimeCompletion)
	}{
		{"missing_commits", func(r *RuntimeCompletion) { r.Commits = nil }},
		{"missing_first_commit", func(r *RuntimeCompletion) { r.Commits = r.Commits[1:] }},
		{"tampered_early_item", func(r *RuntimeCompletion) { r.Commits[0].Items[0].Text = "changed" }},
		{"tampered_early_word", func(r *RuntimeCompletion) { r.Commits[0].Items[0].Words[0].Text = "changed" }},
		{"tampered_early_probability", func(r *RuntimeCompletion) { *r.Commits[0].Items[0].Probability = .8 }},
		{"tampered_early_word_probability", func(r *RuntimeCompletion) { *r.Commits[0].Items[0].Words[0].Probability = .8 }},
		{"missing_cumulative_item", func(r *RuntimeCompletion) { r.Result.Stream.Items = r.Result.Stream.Items[:1] }},
		{"duplicate_cumulative_item", func(r *RuntimeCompletion) {
			r.Result.Stream.Items = append(r.Result.Stream.Items[:1], r.Result.Stream.Items...)
		}},
		{"reordered_cumulative_items", func(r *RuntimeCompletion) {
			r.Result.Stream.Items[0], r.Result.Stream.Items[1] = r.Result.Stream.Items[1], r.Result.Stream.Items[0]
		}},
		{"final_delta_only", func(r *RuntimeCompletion) { r.Result.Stream.Items = r.Result.Stream.Items[1:] }},
		{"changed_empty_words", func(r *RuntimeCompletion) { r.Result.Stream.Items[1].Words = []RuntimeTranscriptWord{} }},
		{"result_wrong_episode", func(r *RuntimeCompletion) { r.Result.Stream.EpisodeNumber++ }},
		{"result_wrong_through", func(r *RuntimeCompletion) { r.Result.Stream.CommittedThroughSec = 2 }},
		{"result_not_final", func(r *RuntimeCompletion) { r.Result.Stream.Final = false }},
		{"reordered_commits", func(r *RuntimeCompletion) { r.Commits[0], r.Commits[1] = r.Commits[1], r.Commits[0] }},
		{"duplicate_revision", func(r *RuntimeCompletion) { r.Commits[1].Revision = r.Commits[0].Revision }},
		{"wrong_episode", func(r *RuntimeCompletion) { r.Commits[0].EpisodeNumber++ }},
		{"early_final", func(r *RuntimeCompletion) { r.Commits[0].Final = true }},
		{"regressed_time", func(r *RuntimeCompletion) { r.Commits[1].CommittedThroughSec = .25 }},
		{"replayed_item", func(r *RuntimeCompletion) { r.Commits[1].Items[0].StartSec = .25 }},
		{"nonfinite_time", func(r *RuntimeCompletion) { r.Commits[0].CommittedThroughSec = math.NaN() }},
		{"result_not_last_commit", func(r *RuntimeCompletion) { r.Result.Stream.Revision++ }},
		{"missing_final", func(r *RuntimeCompletion) { r.Commits[1].Final = false }},
		{"wrong_body_hash", func(r *RuntimeCompletion) { r.Snapshot.BodySHA256 = strings.Repeat("c", 64) }},
		{"wrong_component", func(r *RuntimeCompletion) { r.Result.Component.Version = "other" }},
		{"nonterminal_snapshot", func(r *RuntimeCompletion) { r.Snapshot.State = "running" }},
		{"oversized_receipt", func(r *RuntimeCompletion) {
			first := r.Commits[0]
			first.CommittedThroughSec = 1
			first.Items = nil
			for i := 0; i < 513; i++ {
				first.Items = append(first.Items, RuntimeTranscriptItem{StartSec: float64(i) / 1024, EndSec: float64(i+1) / 1024, Text: strings.Repeat("x", 8192)})
			}
			r.Commits[0] = first
			r.Commits[1].Items = nil
			r.Result.Stream.Items = append([]RuntimeTranscriptItem(nil), first.Items...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("invalid completion accepted")
			}
		})
	}
}

func TestRuntimeAckRouteIsExact(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		for _, suffix := range []string{"job/ack", "job/ack/", "job/ack/extra", "../ack", "job%2fother/ack"} {
			want := method == "POST" && suffix == "job/ack"
			if got := RuntimeRouteAllowed(method, RuntimeJobsPath+"/"+suffix); got != want {
				t.Fatalf("%s %s: got %v want %v", method, suffix, got, want)
			}
		}
	}
}

func TestRuntimeClientRejectsUntrustedSession(t *testing.T) {
	r := NewRuntimeClient(&Client{})
	if _, err := r.Upload(context.Background(), nil, "audio/wav", 44, strings.NewReader(strings.Repeat("x", 44))); err == nil {
		t.Fatal("nil session accepted")
	}
	if _, err := (&Client{}).RuntimeSessionAddress(nil); err == nil {
		t.Fatal("nil address accepted")
	}
}
func TestRuntimeClientStrictSnapshotWire(t *testing.T) {
	for _, raw := range []string{`{"ID":"job","id":"other"}`, `{"ID":"job","ID":"other"}`, `{"ID":null}`, `{"ID":"job","extra":1}`, `{"ID":"job"} {}`, `{"ID":"\xff"}`} {
		var s RuntimeSnapshot
		if runtimeDecode([]byte(raw), &s) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var s RuntimeSnapshot
	if err := runtimeDecode([]byte(`{"ID":"job","State":"queued","StartedAt":"0001-01-01T00:00:00Z"}`), &s); err != nil {
		t.Fatal(err)
	}
	if runtimeStateValid("future-success") || RuntimeState("future-success").Terminal() {
		t.Fatal("unknown state succeeded")
	}
}
func TestRuntimeClientSSESequenceAnd410(t *testing.T) {
	stream := "id: 2\nevent: runtime\ndata: {\"Sequence\":2,\"At\":\"2026-01-01T00:00:00Z\",\"State\":\"running\",\"Code\":\"\",\"Progress\":null,\"Commit\":null}\n\n"
	var count int
	if err := runtimeReadEvents(context.Background(), strings.NewReader(stream), 1, func(e RuntimeEvent) error { count++; return nil }); err != nil || count != 1 {
		t.Fatalf("events %d: %v", count, err)
	}
	for _, bad := range []string{strings.Replace(stream, "id: 2", "id: 3", 1), strings.Replace(stream, "\"Sequence\":2", "\"Sequence\":4", 1), strings.TrimSuffix(stream, "\n\n"), strings.Replace(stream, "runtime", "unknown", 1)} {
		if runtimeReadEvents(context.Background(), strings.NewReader(bad), 1, func(RuntimeEvent) error { return nil }) == nil {
			t.Fatal("bad SSE accepted")
		}
	}
	resp := &http.Response{StatusCode: 410, Header: http.Header{"Content-Type": []string{"application/json"}}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(`{"code":"center_event_cursor_expired"}`))}
	if !errors.Is(runtimeResponseError(resp), ErrRuntimeEventCursorExpired) {
		t.Fatal("410 not stable")
	}
}
func TestRuntimeClientBodyAndResultContracts(t *testing.T) {
	var body RuntimeBody
	if err := runtimeDecode([]byte(`{"component":{"id":"haar-consumer","version":"v1","manifest_sha256":"`+strings.Repeat("a", 64)+`","protocol":"haar/v1"},"face":{"asset_id":"asset"}}`), &body); err != nil || !body.validate("image.face") {
		t.Fatal(err)
	}
	if body.validate("highlight.score") {
		t.Fatal("wrong scene accepted")
	}
	req := runtimeValidationRequest{Binding: ExecutionBindingRef{Scene: "image.face"}, Body: body}
	good := RuntimeResult{Component: body.Component, Face: RuntimeFrontal}
	if !good.validate(req) {
		t.Fatal("valid face rejected")
	}
	good.Component.Version = "other"
	if good.validate(req) {
		t.Fatal("wrong component accepted")
	}
	raw, _ := json.Marshal(body)
	if len(raw) == 0 {
		t.Fatal("body wire")
	}
}

func TestRuntimeStreamOptionsWire(t *testing.T) {
	prefix := `{"component":{"id":"whisper-stream","version":"v1","manifest_sha256":"` + strings.Repeat("a", 64) + `","protocol":"fengshen.whispercpp.stream/v1"},"transcribe":{"asset_id":"audio","stream":true,"vad":false,"offset_ms":0,"episode_number":1`
	options := `{"window_sec":40,"step_sec":1,"stable_rounds":3,"timestamp_tolerance_sec":0,"finalize_delay_sec":0}`
	legacy := prefix + `}}`
	for _, suffix := range []string{"", `,"options":` + options} {
		raw := legacy
		if suffix != "" {
			raw = prefix + suffix + `}}`
		}
		var b RuntimeBody
		if runtimeDecode([]byte(raw), &b) != nil || !b.validate("highlight.transcribe") {
			t.Fatalf("valid options rejected: %s", raw)
		}
		got, _ := json.Marshal(b)
		if string(got) != raw {
			t.Fatalf("wire changed: %s != %s", got, raw)
		}
	}
	for _, bad := range []string{
		`null`, `{}`, strings.Replace(options, `"window_sec":40`, `"window_sec":0`, 1),
		strings.Replace(options, `"step_sec":1`, `"step_sec":0`, 1),
		strings.Replace(options, `"stable_rounds":3`, `"stable_rounds":0`, 1),
		strings.Replace(options, `"step_sec":1`, `"step_sec":11`, 1),
		strings.Replace(options, `"window_sec":40`, `"window_sec":4`, 1),
		strings.Replace(options, `"timestamp_tolerance_sec":0`, `"timestamp_tolerance_sec":-1`, 1),
		strings.Replace(options, `"finalize_delay_sec":0`, `"finalize_delay_sec":11`, 1),
		strings.Replace(options, `"window_sec":40`, `"window_sec":null`, 1),
		strings.Replace(options, `"window_sec":40`, `"window_sec":NaN`, 1),
		strings.Replace(options, `"window_sec":40`, `"window_sec":1e999`, 1),
		strings.Replace(options, `"window_sec":40`, `"Window_sec":40`, 1),
		strings.Replace(options, `"window_sec":40`, `"extra":1,"window_sec":40`, 1),
		strings.Replace(options, `"window_sec":40`, `"window_sec":40,"window_sec":40`, 1),
	} {
		var b RuntimeBody
		if runtimeDecode([]byte(prefix+`,"options":`+bad+`}}`), &b) == nil && b.validate("highlight.transcribe") {
			t.Fatalf("invalid options accepted: %s", bad)
		}
	}
	var b RuntimeBody
	raw := strings.Replace(prefix+`,"options":`+options+`}}`, `"stream":true`, `"stream":false`, 1)
	raw = strings.Replace(raw, "whisper-stream", "whisper-batch", 1)
	if runtimeDecode([]byte(raw), &b) == nil && b.validate("highlight.transcribe") {
		t.Fatal("batch accepted stream options")
	}
}
