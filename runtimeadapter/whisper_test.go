package runtimeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeprocess"
	"strings"
)

func TestWhisperRejectsUndeclaredProtocolBeforeAssets(t *testing.T) {
	for _, stream := range []bool{false, true} {
		id, protocol := "whisper-batch", domain.CenterWhisperBatchProtocol
		if stream {
			id, protocol = "whisper-stream", domain.CenterWhisperStreamProtocol
		}
		for _, declared := range []string{"", "other/v1", protocol} {
			componentProtocol := protocol
			if declared == protocol {
				componentProtocol = "other/v1"
			}
			req := runtimejobs.Request{Body: runtimejobs.Body{Transcribe: &runtimejobs.TranscribeInput{EpisodeNumber: 1, Stream: stream, VAD: true}}}
			req.Body.Component.ID, req.Body.Component.Version, req.Body.Component.Protocol = id, "contract-v1", componentProtocol
			resolved := Resolved{Manifest: components.Manifest{Schema: 1, ComponentID: id, Version: "contract-v1", OS: "windows", Arch: "amd64", Entry: "runner.exe", EntryType: "executable", Packaging: "native", Protocols: []string{declared}, Files: []components.File{{Path: "runner.exe", Bytes: "1", SHA256: strings.Repeat("a", 64)}}}}
			if _, err := (&Executor{}).transcribe(context.Background(), req, resolved, nil); err == nil || err.Error() != "center_protocol_unsupported" {
				t.Fatalf("stream=%v declared=%q requested=%q: %v", stream, declared, componentProtocol, err)
			}
		}
	}
}

func TestWhisperPCMFormatAndBounds(t *testing.T) {
	for _, bad := range []bool{false, true} {
		data := whisperTestWAV(1)
		if bad {
			data[34] = 24
		}
		f, err := os.Create(filepath.Join(t.TempDir(), "input.wav"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(data); err != nil {
			f.Close()
			t.Fatal(err)
		}
		reader, count, err := whisperPCM(f, int64(len(data)))
		if bad {
			if err == nil {
				t.Fatal("accepted non-s16 PCM")
			}
		} else {
			if err != nil || count != 16000 {
				t.Fatalf("PCM: %d %v", count, err)
			}
			n, err := io.Copy(io.Discard, reader)
			if err != nil || n != 32000 {
				t.Fatalf("data bounds: %d %v", n, err)
			}
		}
		f.Close()
	}
}

func TestWhisperBatchStrictMapping(t *testing.T) {
	good := `{"transcription":[{"offsets":{"from":100,"to":900},"text":"你好","tokens":[{"offsets":{"from":100,"to":400},"text":"你","p":0.9},{"offsets":{"from":400,"to":900},"text":"好","p":0.8}]}]}`
	input := runtimejobs.TranscribeInput{EpisodeNumber: 3, OffsetMS: 12000}
	items, err := whisperBatchItems([]byte(good), input, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Words) != 2 || items[0].StartSec != 12.1 || items[0].Words[1].EndSec != 12.9 {
		t.Fatalf("wrong absolute word mapping: %+v", items)
	}
	for _, bad := range []string{
		`{"transcription":[],"path":"evil"}`,
		`{"transcription":[],"transcription":[]}`,
		`{"transcription":[{"offsets":{"from":100,"to":1200},"text":"x","tokens":[]}]}`,
		`{"transcription":[{"offsets":{"from":100,"to":900},"text":"x","tokens":[{"offsets":{"from":100,"to":900},"text":"y"}]}]}`,
		`{"transcription":[{"offsets":{"from":100,"to":900},"text":"x","tokens":[{"offsets":{"from":100,"to":900},"text":"x","p":2}]}]}`,
		`{"transcription":null}`,
		`{"transcription":[]} {}`,
	} {
		if _, err := whisperBatchItems([]byte(bad), input, 1); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestWhisperBatchVADOriginalTimelineStrict(t *testing.T) {
	input := runtimejobs.TranscribeInput{VAD: true, EpisodeNumber: 3, OffsetMS: 12000}
	good := `{"transcription":[{"offsets":{"from":3100,"to":3900},"text":"你好","tokens":[{"offsets":{"from":3100,"to":3400},"text":"你","p":0.9},{"offsets":{"from":3400,"to":3900},"text":"好","p":0.8}]}]}`
	items, err := whisperBatchItems([]byte(good), input, 5)
	if err != nil || len(items) != 1 || items[0].StartSec != 15.1 || items[0].Words[1].EndSec != 15.9 {
		t.Fatalf("original silence gap/offset lost: %+v %v", items, err)
	}
	for _, bad := range []string{
		strings.ReplaceAll(good, "3900", "5100"),
		strings.ReplaceAll(good, "3100", "-100"),
		strings.Replace(good, `"from":3400`, `"from":3300`, 1),
		strings.Replace(good, `"p":0.8`, `"p":1.1`, 1),
	} {
		if _, err := whisperBatchItems([]byte(bad), input, 5); err == nil {
			t.Fatalf("accepted invalid VAD timeline: %s", bad)
		}
	}
}

func TestWhisperCommitOffsetAndConservativeWatermark(t *testing.T) {
	input := runtimejobs.TranscribeInput{Stream: true, EpisodeNumber: 7, OffsetMS: 30000}
	c := transcript.StreamCommit{EpisodeNumber: 7, Revision: 2, CommittedThroughSec: 4, Items: []domain.TranscriptItem{{StartSec: 0.2, EndSec: 1, Text: "你好", Words: []domain.TranscriptWord{{StartSec: 0.2, EndSec: 1, Text: "你好"}}}}}
	first, err := whisperCommit(c, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.CommittedThroughSec != 31 || first.Items[0].Words[0].StartSec != 30.2 {
		t.Fatalf("bad mapping: %+v", first)
	}
	// A later stable item may precede the decoder's previous received-audio boundary.
	c.Revision = 3
	c.Items[0].StartSec = 1.2
	c.Items[0].EndSec = 2
	c.Items[0].Words[0].StartSec = 1.2
	c.Items[0].Words[0].EndSec = 2
	second, err := whisperCommit(c, input, &first)
	if err != nil || second.CommittedThroughSec != 32 {
		t.Fatalf("late stable item rejected: %+v %v", second, err)
	}
	c.Revision = 4
	c.Final = true
	c.Items = []domain.TranscriptItem{}
	final, err := whisperCommit(c, input, &second)
	if err != nil || final.CommittedThroughSec != 34 || !final.Final {
		t.Fatalf("final: %+v %v", final, err)
	}
}

func TestWhisperWireStrict(t *testing.T) {
	good := `{"protocol":"fengshen.whispercpp.stream/v1","type":"hypothesis","request_id":"request","episode_number":2,"revision":1,"committed_through_sec":2,"items":[]}` + "\n"
	msg, err := whisperReadMessage(bytes.NewBufferString(good), "hypothesis", "request", 2, 1)
	if err != nil || *msg.Through != 2 {
		t.Fatalf("good: %+v %v", msg, err)
	}
	for _, bad := range []string{
		`{"protocol":"fengshen.whispercpp.stream/v1","type":"ready","request_id":"request","episode_number":3,"revision":0}` + "\n",
		`{"protocol":"fengshen.whispercpp.stream/v1","type":"hypothesis","request_id":"request","episode_number":2,"revision":1,"committed_through_sec":2,"items":[],"path":"evil"}` + "\n",
		good[:len(good)-1],
		`{"protocol":"fengshen.whispercpp.stream/v1","type":"hypothesis","request_id":"request","episode_number":2,"revision":1,"revision":1,"items":[]}` + "\n",
	} {
		if _, err := whisperReadMessage(bytes.NewBufferString(bad), "hypothesis", "request", 2, 1); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := whisperReadMessage(io.LimitReader(bytes.NewReader(bytes.Repeat([]byte("x"), (1<<20)+1)), (1<<20)+1), "ready", "request", 2, 0); err == nil {
		t.Fatal("accepted oversized line")
	}
}

func TestWhisperNativeErrorSafeDiagnostic(t *testing.T) {
	for _, kind := range []string{"ready", "hypothesis", "final"} {
		for _, code := range []string{"window_unstable", "window_limit", "model_load", "state_init", "vad_model_load", "cancelled"} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				revision := uint64(7)
				if kind == "ready" {
					revision = 0
				}
				frame, err := json.Marshal(whisperWire{Protocol: transcript.StreamProtocolVersion, Type: "error", RequestID: "request", Episode: 2, Revision: revision, ErrorCode: code})
				if err != nil {
					t.Fatal(err)
				}
				msg, err := whisperReadMessage(bytes.NewReader(append(frame, '\n')), kind, "request", 2, revision)
				var diagnostic *whisperInferenceError
				if !errors.As(err, &diagnostic) || diagnostic.code != code {
					t.Fatal("lost safe native classification")
				}
				if msg.Protocol != "" || msg.ErrorCode != "" || err.Error() != "center_result_invalid" || errors.Unwrap(err) != nil {
					t.Fatal("retained frame or changed public error boundary")
				}
				raw, marshalErr := json.Marshal(err)
				if marshalErr != nil || string(raw) != "{}" {
					t.Fatal("private diagnostic crossed JSON boundary")
				}
				if whisperContextError(context.Background(), err) != err {
					t.Fatal("context boundary discarded diagnostic")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if !errors.Is(whisperContextError(ctx, err), context.Canceled) {
					t.Fatal("native error overrode cancellation")
				}
			})
		}
	}
}

func TestWhisperNativeErrorRejectsUnsafeFrames(t *testing.T) {
	good := `{"protocol":"fengshen.whispercpp.stream/v1","type":"error","request_id":"request","episode_number":2,"revision":1,"error_code":"window_unstable"}`
	for name, frame := range map[string]string{
		"unknown-code":            strings.Replace(good, "window_unstable", "unknown_failure", 1),
		"code-path":               strings.Replace(good, "window_unstable", "W:/private/model.bin", 1),
		"code-reason":             strings.Replace(good, "window_unstable", "window_unstable: private reason", 1),
		"code-newline":            strings.Replace(good, "window_unstable", `window_unstable\nprivate`, 1),
		"code-case":               strings.Replace(good, "window_unstable", "WINDOW_UNSTABLE", 1),
		"code-space":              strings.Replace(good, "window_unstable", "window_unstable ", 1),
		"empty-code":              strings.Replace(good, "window_unstable", "", 1),
		"null-code":               strings.Replace(good, `"window_unstable"`, "null", 1),
		"numeric-code":            strings.Replace(good, `"window_unstable"`, "123", 1),
		"wrong-protocol":          strings.Replace(good, "fengshen.whispercpp.stream/v1", "other/v1", 1),
		"wrong-request":           strings.Replace(good, `"request"`, `"other"`, 1),
		"wrong-episode":           strings.Replace(good, `"episode_number":2`, `"episode_number":3`, 1),
		"wrong-revision":          strings.Replace(good, `"revision":1`, `"revision":2`, 1),
		"missing-episode":         strings.Replace(good, `"episode_number":2,`, "", 1),
		"missing-revision":        strings.Replace(good, `"revision":1,`, "", 1),
		"duplicate-code":          strings.Replace(good, `"error_code":`, `"error_code":"model_load","error_code":`, 1),
		"duplicate-identity":      strings.Replace(good, `"revision":1`, `"revision":2,"revision":1`, 1),
		"folded-code":             strings.Replace(good, "error_code", "Error_Code", 1),
		"text-field":              strings.TrimSuffix(good, "}") + `,"text":"private corpus"}`,
		"reason-field":            strings.TrimSuffix(good, "}") + `,"reason":"private cause"}`,
		"extra-items":             strings.TrimSuffix(good, "}") + `,"items":[]}`,
		"extra-final":             strings.TrimSuffix(good, "}") + `,"final":false}`,
		"non-error-type":          strings.Replace(good, `"type":"error"`, `"type":"hypothesis"`, 1),
		"trailing-json":           good + `{}`,
		"invalid-utf8":            strings.Replace(good, "window_unstable", "\xff", 1),
		"native-missing-identity": `{"protocol":"fengshen.whispercpp.stream/v1","type":"error","request_id":"request","error_code":"window_unstable"}`,
	} {
		t.Run(name, func(t *testing.T) {
			msg, err := whisperReadMessage(strings.NewReader(frame+"\n"), "hypothesis", "request", 2, 1)
			var diagnostic *whisperInferenceError
			if err != errWhisperResult || errors.As(err, &diagnostic) || msg.ErrorCode != "" {
				t.Fatal("unsafe native error was not generically rejected")
			}
		})
	}
	// Revision zero must be present even while waiting for ready.
	missing := strings.Replace(good, `"revision":1,`, "", 1)
	if _, err := whisperReadMessage(strings.NewReader(missing+"\n"), "ready", "request", 2, 0); err != errWhisperResult {
		t.Fatal("accepted missing zero revision")
	}
}

func TestWhisperRejectsInvalidOrBatchOptionsBeforeAssets(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mutate := range []func(*integration.RuntimeStreamOptions){
			func(o *integration.RuntimeStreamOptions) { o.WindowSec = 0 },
			func(o *integration.RuntimeStreamOptions) { o.StepSec = 0 },
			func(o *integration.RuntimeStreamOptions) { o.StableRounds = 0 },
			func(o *integration.RuntimeStreamOptions) { o.WindowSec = math.NaN() },
			func(o *integration.RuntimeStreamOptions) { o.FinalizeDelaySec = math.Inf(1) },
			func(o *integration.RuntimeStreamOptions) {
				if stream {
					o.StepSec = 11
				}
			},
		} {
			o := integration.RuntimeStreamOptions(transcript.DefaultStreamOptions())
			mutate(&o)
			req := runtimejobs.Request{Body: runtimejobs.Body{Transcribe: &runtimejobs.TranscribeInput{Stream: stream, EpisodeNumber: 1, Options: &o}}}
			if _, err := (&Executor{}).transcribe(context.Background(), req, Resolved{}, nil); err == nil || err.Error() != "center_request_invalid" {
				t.Fatalf("invalid/batch options reached assets: %v", err)
			}
		}
	}
}

func TestWhisperBatchVADAdmissionBeforeLaunch(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"4979e04-batch-v1", "4979e04-batch-v2", "unknown", "4979e04-batch-v2-extra"} {
		for _, vad := range []bool{false, true} {
			for _, bound := range []bool{false, true} {
				resolved, req := whisperTestResolved(t, exe, false)
				resolved.Manifest.Version, req.Body.Component.Version = version, version
				resolved.Entry, err = runtimeprocess.VerifyBinary(exe, resolved.Manifest.Files[0].SHA256, "whisper-batch/"+version, req.Binding.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				req.Body.Transcribe.VAD = vad
				if bound {
					resolved.Models = append(resolved.Models, ModelFile{ModelID: "whisper-vad", Path: filepath.Join(t.TempDir(), "trusted-vad.bin")})
				}
				want := "center_asset_forbidden"
				if vad && version != "4979e04-batch-v2" {
					want = "center_whisper_vad_unsupported"
				} else if vad && !bound {
					want = "center_binding_missing"
				}
				if _, err := (&Executor{}).transcribe(context.Background(), req, resolved, nil); err == nil || err.Error() != want {
					t.Errorf("%s vad=%v bound=%v: %v want %s", version, vad, bound, err, want)
				}
			}
		}
	}
}

func TestWhisperVADAndCancellationRejectBeforeAssets(t *testing.T) {
	e := &Executor{}
	req := runtimejobs.Request{Body: runtimejobs.Body{Transcribe: &runtimejobs.TranscribeInput{VAD: true, EpisodeNumber: 1}}}
	if _, err := e.transcribe(context.Background(), req, Resolved{}, nil); err == nil || err.Error() != "center_protocol_unsupported" {
		t.Fatalf("VAD: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.transcribe(ctx, req, Resolved{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestWhisperVADBindingsBeforeAssets(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		vad   bool
		extra []ModelFile
		want  string
	}{
		{"enabled-missing", true, nil, "center_binding_missing"},
		{"enabled-bound", true, []ModelFile{{ModelID: "whisper-vad", Path: filepath.Join(t.TempDir(), "vad.bin")}}, "center_asset_forbidden"},
		{"disabled-bound", false, []ModelFile{{ModelID: "whisper-vad", Path: filepath.Join(t.TempDir(), "vad.bin")}}, "center_asset_forbidden"},
		{"relative", false, []ModelFile{{ModelID: "whisper-vad", Path: "vad.bin"}}, "center_binding_stale"},
		{"duplicate", true, []ModelFile{{ModelID: "whisper-vad", Path: exe}, {ModelID: "whisper-vad", Path: exe}}, "center_binding_stale"},
		{"duplicate-asr", false, []ModelFile{{ModelID: "whisper-asr", Path: exe}}, "center_binding_stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, req := whisperTestResolved(t, exe, true)
			req.Body.Transcribe.VAD = tc.vad
			resolved.Models = append(resolved.Models, tc.extra...)
			if _, err := (&Executor{}).transcribe(context.Background(), req, resolved, nil); err == nil || err.Error() != tc.want {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
}

func TestWhisperCommitRejectsBadProbability(t *testing.T) {
	p := 1.1
	c := transcript.StreamCommit{EpisodeNumber: 1, Revision: 1, CommittedThroughSec: 1, Final: true, Items: []domain.TranscriptItem{{StartSec: 0, EndSec: 1, Text: "x", Probability: &p, Words: []domain.TranscriptWord{{StartSec: 0, EndSec: 1, Text: "x"}}}}}
	if _, err := whisperCommit(c, runtimejobs.TranscribeInput{Stream: true, EpisodeNumber: 1}, nil); err == nil {
		t.Fatal("accepted probability")
	}
	raw, _ := json.Marshal(c)
	if bytes.Contains(raw, []byte("path")) {
		t.Fatal("path in projection")
	}
}
