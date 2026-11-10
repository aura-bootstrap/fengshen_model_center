package highlightclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

func TestStreamOptionsContractMatchesTranscript(t *testing.T) {
	defaults := transcript.DefaultStreamOptions()
	// Explicit wire options are already normalized; unlike transcript.Validate,
	// wire validation must reject zero window, step and rounds rather than default.
	for _, field := range []string{"window", "step", "rounds", "tolerance", "delay"} {
		for _, value := range []float64{-1, 0, .1, .5, 1, 2, 4.9, 5, 10, 120, 121, math.NaN(), math.Inf(1)} {
			o := defaults
			explicitZero := false
			switch field {
			case "window":
				o.WindowSec = value
				explicitZero = value == 0
			case "step":
				o.StepSec = value
				explicitZero = value == 0
			case "rounds":
				if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
					continue
				}
				o.StableRounds = int(value)
				explicitZero = value == 0
			case "tolerance":
				o.TimestampToleranceSec = value
			case "delay":
				o.FinalizeDelaySec = value
			}
			wire := integration.RuntimeStreamOptions(o)
			want := !explicitZero && o.Validate() == nil
			if (wire.Validate() == nil) != want {
				t.Fatalf("contract drift %s=%v: %+v", field, value, o)
			}
		}
	}
	o := defaults
	o.WindowSec, o.StepSec = 5, 6
	if integration.RuntimeStreamOptions(o).Validate() == nil {
		t.Fatal("step exceeds window")
	}
}

func TestAudioTextJSONProjectionIsLossless(t *testing.T) {
	for _, chain := range []domain.AnalysisChain{domain.AnalysisChainAudioTextV1, domain.AnalysisChainAudioTextStreamV1} {
		c, f, _ := setup(t, "highlight.score", "llama-runtime")
		in := candidate()
		in.AnalysisChain = chain
		in.EligibleUnits = []domain.SpeechUnit{{ID: "unit1", StartSec: 0, EndSec: 1, Text: "text", AlignmentReliable: true, Words: []domain.TranscriptWord{{StartSec: 0, EndSec: 1, Text: "text"}}}}
		in.AudioFeatures = &domain.CandidateAudioFeatures{Schema: domain.AudioFeatureSchemaV1, Window: domain.AudioInterval{StartSec: 0, EndSec: 1}, VAD: domain.VADStatus{Backend: "energy_v1", Reliable: true}, Speech: []domain.AudioInterval{{StartSec: 0, EndSec: 1}}, Silence: []domain.AudioInterval{}, Pauses: []domain.AudioInterval{}, Overlaps: []domain.OverlapInterval{}, Loudness: domain.LoudnessMetrics{Reason: "metrics_unavailable"}, SpeechRate: domain.SpeechRateMetrics{Reason: "metrics_unavailable"}, Pitch: domain.PitchMetrics{Reason: "metrics_unavailable"}, UnitMetrics: []domain.UnitAudioMetrics{}, SoundEvents: []domain.SoundEvent{}}
		if _, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), in); e != nil {
			t.Fatal(e)
		}
		want := in
		want.RequestID = f.ids[0]
		want.TaskID = "task"
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(f.body.Score)
		var left, right any
		if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil || !reflect.DeepEqual(left, right) {
			t.Fatalf("lossy JSON projection:\n%s\n%s", a, b)
		}
		if len(f.uploaded) != 0 || f.body.Vision != nil {
			t.Fatal("text chain used vision")
		}
	}
}
func TestUploadErrorsAndImageReceiptMismatchPropagate(t *testing.T) {
	sentinel := errors.New("upload transport failed")
	for _, audioInput := range []bool{false, true} {
		c, f, _ := setup(t, "highlight.vision", "llama-runtime")
		if audioInput {
			c, f, _ = setup(t, "highlight.transcribe", "whisper-batch")
		}
		f.uploadErr = sentinel
		var e error
		if audioInput {
			ep, p := prepared(t)
			_, e = c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, nil)
		} else {
			in := candidate()
			in.Image = imageInput(t)
			_, e = c.Scorer("highlight.vision").ScoreEvents(context.Background(), in)
		}
		if !errors.Is(e, sentinel) || len(f.ids) != 0 {
			t.Fatalf("upload error lost or work submitted: %v", e)
		}
	}
	for _, field := range []string{"sha", "dimensions", "bytes"} {
		c, f, _ := setup(t, "highlight.vision", "llama-runtime")
		f.corrupt = func(a *integration.RuntimeAsset) {
			switch field {
			case "sha":
				a.Metadata.SHA256 = "bad"
			case "dimensions":
				a.Metadata.Width++
			case "bytes":
				a.Metadata.Bytes = "1"
			}
		}
		in := candidate()
		in.Image = imageInput(t)
		if _, e := c.Scorer("highlight.vision").ScoreEvents(context.Background(), in); e == nil || len(f.ids) != 0 {
			t.Fatal("image receipt mismatch ignored")
		}
	}
}
func TestImageDimensionsAndDecodedFileLimit(t *testing.T) {
	c, f, _ := setup(t, "highlight.vision", "llama-runtime")
	var buf bytes.Buffer
	if e := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4097, 1))); e != nil {
		t.Fatal(e)
	}
	in := candidate()
	in.Image = imageInput(t)
	in.Image.DataBase64 = base64.StdEncoding.EncodeToString(buf.Bytes())
	if _, e := c.Scorer("highlight.vision").ScoreEvents(context.Background(), in); e == nil {
		t.Fatal("oversize dimensions accepted")
	}
	path := filepath.Join(t.TempDir(), "oversize.png")
	file, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = file.Truncate(maxImageBytes + 1); e != nil {
		t.Fatal(e)
	}
	file.Close()
	in.Image = nil
	in.ContactSheetPath = path
	if _, e = c.Scorer("highlight.vision").ScoreEvents(context.Background(), in); e == nil {
		t.Fatal("oversize file accepted")
	}
	if len(f.calls) > 0 {
		t.Fatal(f.calls)
	}
}
func TestWAVStructuralChangesFailBeforeUpload(t *testing.T) {
	for _, kind := range []string{"format", "truncated", "unknown-chunk", "data-count"} {
		t.Run(kind, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
			ep, p := prepared(t)
			raw, e := os.ReadFile(p.WAVPath)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "format":
				raw[34] = 8
			case "truncated":
				raw = raw[:len(raw)-1]
			case "unknown-chunk":
				copy(raw[12:], "EVIL")
			case "data-count":
				raw[40]--
			}
			sum := sha256.Sum256(raw)
			p.ContentSHA256 = hex.EncodeToString(sum[:])
			if e = os.WriteFile(p.WAVPath, raw, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, nil); e == nil {
				t.Fatal("bad WAV accepted")
			}
			if len(f.calls) > 0 {
				t.Fatal(f.calls)
			}
		})
	}
}
func TestRecordErrorsAtTerminalAndCancellationAreReturned(t *testing.T) {
	sentinel := errors.New("receipt persistence failed")
	c, _, _ := setup(t, "highlight.score", "llama-runtime")
	count := 0
	c.record = func(integration.RuntimeSnapshot) error {
		count++
		if count == 2 {
			return sentinel
		}
		return nil
	}
	if _, e := c.Scorer("highlight.score").ScoreEvents(context.Background(), candidate()); !errors.Is(e, sentinel) {
		t.Fatalf("terminal callback lost: %v", e)
	}
	c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
	ep, p := prepared(t)
	f.events = commits()
	c.record = func(s integration.RuntimeSnapshot) error {
		if s.State == "cancelled" {
			return sentinel
		}
		return nil
	}
	emitErr := errors.New("stop publication")
	e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { return emitErr })
	if !errors.Is(e, emitErr) || !errors.Is(e, sentinel) {
		t.Fatalf("cancellation callback error lost: %v", e)
	}
}
func TestStreamCommitOrderIndependentOfEventSequence(t *testing.T) {
	for _, kind := range []string{"revision-duplicate", "revision-backwards", "final-twice", "time-backwards", "no-speech"} {
		t.Run(kind, func(t *testing.T) {
			c, f, _ := setup(t, "highlight.transcribe", "whisper-stream")
			ep, p := prepared(t)
			f.events = commits()
			switch kind {
			case "revision-duplicate":
				f.events[1].Commit.Revision = 1
			case "revision-backwards":
				f.events[0].Commit.Revision = 3
			case "final-twice":
				last := *f.events[1].Commit
				f.events = append(f.events, integration.RuntimeEvent{Sequence: 3, Commit: &last})
			case "time-backwards":
				f.events[1].Commit.CommittedThroughSec = .4
			case "no-speech":
				f.events[0].Commit.Items = nil
				f.events[1].Commit.Items = nil
			}
			f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-stream").Component, Stream: cumulativeStream(f.events)}
			if e := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { return nil }); e == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}
func TestEOFRequiresQueryAndProgressIsMapped(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
	ep, p := prepared(t)
	f.eventsErr = io.EOF
	f.events = []integration.RuntimeEvent{{Sequence: 1, Progress: &integration.RuntimeProgress{Stage: "running", Completed: 1, Total: 2}}}
	f.result = integration.RuntimeResult{Component: testBinding("highlight.transcribe", "whisper-batch").Component, Items: []integration.RuntimeTranscriptItem{{StartSec: 0, EndSec: 1, Text: "text"}}}
	calls := 0
	_, e := c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, func(stage string, percent int) {
		calls++
		if stage != "running" || percent != 50 {
			t.Fatalf("bad progress %s %d", stage, percent)
		}
	})
	if e != nil || calls != 1 {
		t.Fatalf("EOF/progress: %v %d", e, calls)
	}
}
func TestBatchProgressCancellationUsesIndependentCancel(t *testing.T) {
	c, f, _ := setup(t, "highlight.transcribe", "whisper-batch")
	ep, p := prepared(t)
	f.events = []integration.RuntimeEvent{{Sequence: 1, Progress: &integration.RuntimeProgress{Stage: "running", Completed: 1, Total: 2}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := c.Transcriber(false).TranscribeWithAudio(ctx, ep, p, func(string, int) { cancel() })
	if !errors.Is(err, context.Canceled) || !f.cancelContextOK || f.calls[len(f.calls)-1] != "cancel" {
		t.Fatalf("batch cancellation not propagated: %v, %v", err, f.calls)
	}
}

func TestMissingBindingsNeverFallback(t *testing.T) {
	c, f, _ := setup(t, "highlight.score", "llama-runtime")
	if _, err := c.Scorer("highlight.vision").ScoreEvents(context.Background(), candidate()); err == nil {
		t.Fatal("vision fell back to text")
	}
	ep, p := prepared(t)
	if _, err := c.Transcriber(false).TranscribeWithAudio(context.Background(), ep, p, nil); err == nil {
		t.Fatal("batch accepted missing binding")
	}
	if err := c.StreamingTranscriber(false).TranscribeStream(context.Background(), ep, p, transcript.DefaultStreamOptions(), func(transcript.StreamCommit) error { return nil }); err == nil {
		t.Fatal("stream accepted missing binding")
	}
	if len(f.calls) != 0 {
		t.Fatal(f.calls)
	}
}

func TestPrecancelledOperationsDoNoIO(t *testing.T) {
	c, f, _ := setup(t, "highlight.score", "llama-runtime")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Scorer("highlight.score").ScoreEvents(ctx, candidate()); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if len(f.calls) != 0 {
		t.Fatal(f.calls)
	}
}
