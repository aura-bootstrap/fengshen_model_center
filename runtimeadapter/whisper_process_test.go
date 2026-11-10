package runtimeadapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeprocess"
)

// The contract helper is this Go test executable, not an ASR implementation.
// Only the supervisor launches it; normal tests never enter this branch.
func init() {
	if len(os.Args) < 2 || (os.Args[1] != "--fengshen-stream-v1" && os.Args[1] != "-m") {
		return
	}
	if os.Args[1] == "-m" {
		output := ""
		for i := 1; i+1 < len(os.Args); i++ {
			if os.Args[i] == "-of" {
				output = os.Args[i+1]
			}
		}
		if output == "" {
			os.Exit(2)
		}
		model := os.Args[2]
		want := []string{"-m", model, "-f", filepath.Join(filepath.Dir(output), "input.wav"), "-l", "zh", "-t", "4", "-ml", "1", "-sow", "-ojf", "-of", output, "-np", "-ng"}
		if strings.HasPrefix(filepath.Base(model), "vad-on-") {
			want = append(want, "--vad", "--vad-model", filepath.Join(filepath.Dir(model), "trusted-vad.bin"))
		}
		if !reflect.DeepEqual(os.Args[1:], want) {
			os.Exit(10)
		}
		err := os.WriteFile(output+".json", []byte(`{"transcription":[{"offsets":{"from":100,"to":900},"text":"你好","tokens":[{"offsets":{"from":100,"to":400},"text":"你","p":0.9},{"offsets":{"from":400,"to":900},"text":"好","p":0.8}]}]}`), 0600)
		if err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString(`{"path":"must-not-be-used"}`)
		os.Exit(0)
	}
	// A binding filename carries the expected VAD argv, independently of start JSON.
	model, vadModel, vadCount, vadModelCount := "", "", 0, 0
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--model":
			i++
			model = os.Args[i]
		case "--vad":
			vadCount++
		case "--vad-model":
			i++
			vadModel = os.Args[i]
			vadModelCount++
		}
	}
	if strings.HasPrefix(filepath.Base(model), "vad-on-") {
		if vadCount != 1 || vadModelCount != 1 || vadModel != filepath.Join(filepath.Dir(model), "trusted-vad.bin") {
			os.Exit(10)
		}
	} else if vadCount != 0 || vadModelCount != 0 {
		os.Exit(11)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	var receivedOptions transcript.StreamOptions
	for scanner.Scan() {
		var m map[string]any
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(4)
		}
		response := map[string]any{"protocol": m["protocol"], "request_id": m["request_id"], "episode_number": m["episode_number"], "revision": m["revision"]}
		switch m["type"] {
		case "start":
			raw, _ := json.Marshal(m["options"])
			if json.Unmarshal(raw, &receivedOptions) != nil || receivedOptions.Validate() != nil {
				os.Exit(8)
			}
			// The model filename carries the independently expected start options.
			for i := 1; i+1 < len(os.Args); i++ {
				if os.Args[i] == "--model" && strings.HasPrefix(filepath.Base(os.Args[i+1]), "options-") {
					want, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(os.Args[i+1]), "options-"), ".bin"))
					var expected transcript.StreamOptions
					if err != nil || json.Unmarshal(want, &expected) != nil || expected != receivedOptions {
						os.Exit(9)
					}
				}
			}
			response["type"] = "ready"
			response["revision"] = 0
		case "audio", "finalize":
			response["type"] = "hypothesis"
			response["committed_through_sec"] = m["end_sec"]
			start, end := .1, .9
			// This case requires the custom tolerance to retain the first stable round.
			if receivedOptions.TimestampToleranceSec == .7 && m["revision"] != float64(1) {
				start, end = .5, 1.3
			}
			response["items"] = []any{map[string]any{"start_sec": start, "end_sec": end, "text": "你好", "words": []any{map[string]any{"start_sec": start, "end_sec": end, "text": "你好"}}}}
			if strings.Contains(filepath.Base(model), "cumulative") {
				count := int(m["revision"].(float64))
				if m["type"] == "finalize" {
					count--
				}
				if count > 3 {
					count = 3
				}
				items := make([]domain.TranscriptItem, count)
				probability := .8
				for i := range items {
					start, end := float64(i)*2+.1, float64(i)*2+.9
					items[i] = domain.TranscriptItem{StartSec: start, EndSec: end, Text: "你好", Probability: &probability, Words: []domain.TranscriptWord{{StartSec: start, EndSec: end, Text: "你好", Probability: &probability}}}
				}
				response["items"] = items
			}
			if strings.Contains(filepath.Base(model), "silence") {
				response["items"] = []any{}
			}
			if m["type"] == "finalize" {
				response["type"] = "final"
				response["final"] = true
				response["committed_through_sec"] = m["committed_through_sec"]
			}
		default:
			os.Exit(5)
		}
		if enc.Encode(response) != nil {
			os.Exit(6)
		}
		if m["type"] == "finalize" {
			os.Exit(0)
		}
	}
	os.Exit(7)
}

func whisperTestWAV(seconds int) []byte {
	n := seconds * 32000
	b := make([]byte, n+44)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(n+36))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	return b
}

func whisperTestResolved(t *testing.T, exe string, stream bool) (Resolved, runtimejobs.Request) {
	t.Helper()
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(b)
	id := "whisper-batch"
	protocols := []string{domain.CenterWhisperBatchProtocol}
	if stream {
		id = "whisper-stream"
		protocols = []string{"fengshen.whispercpp.stream/v1"}
	}
	component := integration.ExecutionComponentRef{ID: id, Version: "contract-v1", Protocol: protocols[0]}
	req := runtimejobs.Request{TaskID: "task", SessionID: "session", Binding: integration.ExecutionBindingRef{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("a", 64)}, Body: runtimejobs.Body{Component: component, Transcribe: &runtimejobs.TranscribeInput{EpisodeNumber: 4, Stream: stream, OffsetMS: 10000}}}
	m := components.Manifest{Schema: 1, ComponentID: id, Version: component.Version, OS: "windows", Arch: "amd64", EntryType: "executable", Entry: filepath.Base(exe), Packaging: "native", Protocols: protocols, Files: []components.File{{Path: filepath.Base(exe), Bytes: strconv.Itoa(len(b)), SHA256: hex.EncodeToString(hash[:])}}}
	entry, err := runtimeprocess.VerifyBinary(exe, hex.EncodeToString(hash[:]), id+"/"+component.Version, req.Binding.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return Resolved{Manifest: m, ComponentRoot: filepath.Dir(exe), Entry: entry, Models: []ModelFile{{ModelID: "whisper-asr", Path: filepath.Join(t.TempDir(), "model.bin")}}}, req
}

func TestWhisperBatchV2VADFixedArguments(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, vad := range []bool{false, true} {
		store, err := runtimeassets.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		e := &Executor{assets: store, scratch: t.TempDir()}
		defer e.supervisor.Close()
		resolved, req := whisperTestResolved(t, exe, false)
		resolved.Manifest.Version, req.Body.Component.Version = "4979e04-batch-v2", "4979e04-batch-v2"
		resolved.Entry, err = runtimeprocess.VerifyBinary(exe, resolved.Manifest.Files[0].SHA256, "whisper-batch/4979e04-batch-v2", req.Binding.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		modelDir := t.TempDir()
		name := "vad-off-model.bin"
		if vad {
			name = "vad-on-model.bin"
		}
		resolved.Models = []ModelFile{{ModelID: "whisper-asr", Path: filepath.Join(modelDir, name)}, {ModelID: "whisper-vad", Path: filepath.Join(modelDir, "trusted-vad.bin")}}
		req.Body.Transcribe.VAD = vad
		wav := whisperTestWAV(6)
		a, err := store.Upload(context.Background(), req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(wav)), bytes.NewReader(wav))
		if err != nil {
			t.Fatal(err)
		}
		req.Body.Transcribe.AssetID = a.ID()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, err := e.transcribe(ctx, req, resolved, nil)
		cancel()
		if err != nil {
			t.Fatalf("vad=%v: %v", vad, err)
		}
		if len(result.Items) != 1 || result.Items[0].StartSec != 10.1 || result.Items[0].Words[1].EndSec != 10.9 {
			t.Fatalf("original timeline lost: %+v", result)
		}
	}
}

func TestWhisperSupervisedBatchAndStream(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			store, err := runtimeassets.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			e := &Executor{assets: store, scratch: t.TempDir()}
			defer e.supervisor.Close()
			resolved, req := whisperTestResolved(t, exe, stream)
			wav := whisperTestWAV(6)
			a, err := store.Upload(context.Background(), req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(wav)), bytes.NewReader(wav))
			if err != nil {
				t.Fatal(err)
			}
			req.Body.Transcribe.AssetID = a.ID()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var commits []runtimejobs.StreamCommit
			result, err := e.transcribe(ctx, req, resolved, func(p runtimejobs.Progress) error {
				if p.Commit != nil {
					commits = append(commits, *p.Commit)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if stream {
				if len(commits) < 2 || result.Stream == nil || !result.Stream.Final || result.Stream.CommittedThroughSec != 16 {
					t.Fatalf("stream: %+v %+v", result, commits)
				}
				found := false
				for _, c := range commits {
					for _, i := range c.Items {
						found = true
						if i.StartSec != 10.1 || i.Words[0].EndSec != 10.9 {
							t.Fatalf("offset lost: %+v", i)
						}
					}
				}
				if !found {
					t.Fatal("no real helper commit")
				}
			} else if len(result.Items) != 1 || len(result.Items[0].Words) != 2 {
				t.Fatalf("batch: %+v", result)
			}
			entries, err := os.ReadDir(e.scratch)
			if err != nil || len(entries) != 0 {
				t.Fatalf("scratch leak: %v %v", entries, err)
			}
			if stream {
				cancelCtx, stop := context.WithCancel(ctx)
				_, cancelErr := e.transcribe(cancelCtx, req, resolved, func(runtimejobs.Progress) error { stop(); return context.Canceled })
				stop()
				if cancelErr != context.Canceled {
					t.Fatalf("live cancellation: %v", cancelErr)
				}
				left, err := os.ReadDir(e.scratch)
				if err != nil || len(left) != 0 {
					t.Fatalf("cancel scratch leak: %v %v", left, err)
				}
				marker := errors.New("consumer_backpressure_stop")
				_, emitErr := e.transcribe(ctx, req, resolved, func(runtimejobs.Progress) error { return marker })
				if emitErr != marker {
					t.Fatalf("emit error lost: %v", emitErr)
				}
			}
			req.TaskID = "other-task"
			if _, err := e.transcribe(ctx, req, resolved, func(runtimejobs.Progress) error { return nil }); err == nil {
				t.Fatal("cross-task access")
			}
		})
	}
}

// Exercise the production adapter and job validators without loading an ASR model.
func TestWhisperRuntimeJobsCumulativeResult(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                       string
		seconds, items, finalItems int
		offset                     int64
		vad                        bool
	}{
		{"cumulative-final-suffix", 6, 3, 1, 10000, true},
		{"cumulative-empty-final", 8, 3, 0, 0, true},
		{"final-only", 2, 1, 1, 10000, false},
		{"silence", 2, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := runtimeassets.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			resolved, req := whisperTestResolved(t, exe, true)
			req.Body.Component.ManifestSHA256 = strings.Repeat("b", 64)
			req.Body.Transcribe.OffsetMS = tc.offset
			req.Body.Transcribe.VAD = tc.vad
			resolved.Binding.Ref, resolved.Binding.Component = req.Binding, req.Body.Component
			name := "vad-off-" + tc.name + ".bin"
			if tc.vad {
				name = "vad-on-" + tc.name + ".bin"
			}
			resolved.Models[0].Path = filepath.Join(t.TempDir(), name)
			if tc.vad {
				resolved.Models = append(resolved.Models, ModelFile{ModelID: "whisper-vad", Path: filepath.Join(filepath.Dir(resolved.Models[0].Path), "trusted-vad.bin")})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			wav := whisperTestWAV(tc.seconds)
			a, err := store.Upload(ctx, req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(wav)), bytes.NewReader(wav))
			if err != nil {
				t.Fatal(err)
			}
			req.Body.Transcribe.AssetID = a.ID()
			e, err := New(func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) (Resolved, error) {
				return resolved, nil
			}, store, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager, err := runtimejobs.New(e, runtimejobs.Options{})
			if err != nil {
				e.Close()
				t.Fatal(err)
			}
			defer manager.Close()
			job, err := manager.Submit(ctx, req.SessionID, req.TaskID, "stream", req.Binding, req.Body)
			if err != nil {
				t.Fatal(err)
			}
			var cursor uint64
			var commits []runtimejobs.StreamCommit
			var items []runtimejobs.TranscriptItem
			var terminal runtimejobs.Event
			for !terminal.State.Terminal() {
				events, next, err := manager.WaitEvents(ctx, req.SessionID, req.TaskID, job.ID, cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Sequence <= cursor {
						t.Fatal("nonmonotonic event sequence")
					}
					cursor = event.Sequence
					if event.Commit != nil {
						commits = append(commits, *event.Commit)
						items = append(items, event.Commit.Items...)
					}
					if event.State.Terminal() {
						terminal = event
					}
				}
				if next != cursor {
					t.Fatal("event cursor mismatch")
				}
			}
			snapshot, err := manager.Snapshot(req.SessionID, req.TaskID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.State != runtimejobs.Succeeded || snapshot.State != runtimejobs.Succeeded {
				t.Fatalf("state=%s code=%s commits=%d committed_items=%d", snapshot.State, snapshot.Code, len(commits), len(items))
			}
			if len(commits) == 0 || !commits[len(commits)-1].Final || len(commits[len(commits)-1].Items) != tc.finalItems || len(items) != tc.items {
				t.Fatalf("unexpected delta history: commits=%d items=%d", len(commits), len(items))
			}
			result, err := manager.Result(req.SessionID, req.TaskID, job.ID)
			if err != nil || result.Stream == nil || result.Component != req.Body.Component {
				t.Fatalf("result: %+v %v", result, err)
			}
			want := commits[len(commits)-1]
			want.Items = items
			if len(items) == 0 {
				want.Items = []runtimejobs.TranscriptItem{}
			}
			if !reflect.DeepEqual(*result.Stream, want) || result.Stream.CommittedThroughSec != float64(tc.offset)/1000+float64(tc.seconds) {
				t.Fatalf("result does not equal complete delta history: %+v", result.Stream)
			}
			if tc.items > 0 && (result.Stream.Items[0].StartSec != float64(tc.offset)/1000+.1 || len(result.Stream.Items[0].Words) != 1) {
				t.Fatal("source offset or words lost")
			}
			t.Logf("state=%s commits=%d cumulative_items=%d final_delta_items=%d", snapshot.State, len(commits), len(result.Stream.Items), tc.finalItems)
		})
	}
}

func TestWhisperVADArgvAndSilence(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, vad := range []bool{false, true} {
		store, err := runtimeassets.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		e := &Executor{assets: store, scratch: t.TempDir()}
		resolved, req := whisperTestResolved(t, exe, true)
		name := "vad-off-silence.bin"
		if vad {
			name = "vad-on-silence.bin"
		}
		resolved.Models[0].Path = filepath.Join(t.TempDir(), name)
		resolved.Models = append(resolved.Models, ModelFile{ModelID: "whisper-vad", Path: filepath.Join(filepath.Dir(resolved.Models[0].Path), "trusted-vad.bin")})
		req.Body.Transcribe.VAD = vad
		wav := whisperTestWAV(2)
		a, err := store.Upload(context.Background(), req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(wav)), bytes.NewReader(wav))
		if err != nil {
			t.Fatal(err)
		}
		req.Body.Transcribe.AssetID = a.ID()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, err := e.transcribe(ctx, req, resolved, func(p runtimejobs.Progress) error {
			if p.Commit != nil && len(p.Commit.Items) != 0 {
				t.Error("silence invented speech")
			}
			return nil
		})
		cancel()
		e.supervisor.Close()
		store.Close()
		if err != nil || result.Stream == nil || !result.Stream.Final || result.Stream.CommittedThroughSec != 12 {
			t.Fatalf("VAD=%v: %+v %v", vad, result, err)
		}
	}
}

func TestWhisperCustomStreamOptionsReachNativeAndCommitter(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []*transcript.StreamOptions{nil,
		{WindowSec: 40, StepSec: 1, StableRounds: 1, TimestampToleranceSec: 0, FinalizeDelaySec: 0},
		{WindowSec: 10, StepSec: .5, StableRounds: 3, TimestampToleranceSec: .7, FinalizeDelaySec: 0},
		{WindowSec: 60, StepSec: 4, StableRounds: 1, TimestampToleranceSec: .4, FinalizeDelaySec: 5},
	} {
		want := transcript.DefaultStreamOptions()
		if options != nil {
			want = *options
		}
		raw, _ := json.Marshal(want)
		t.Run(string(raw), func(t *testing.T) {
			store, err := runtimeassets.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			e := &Executor{assets: store, scratch: t.TempDir()}
			defer e.supervisor.Close()
			resolved, req := whisperTestResolved(t, exe, true)
			resolved.Models[0].Path = filepath.Join(t.TempDir(), "options-"+hex.EncodeToString(raw)+".bin")
			if options != nil {
				wire, _ := json.Marshal(req.Body)
				wire = []byte(strings.TrimSuffix(string(wire), "}}") + `,"options":` + string(raw) + `}}`)
				if json.Unmarshal(wire, &req.Body) != nil {
					t.Fatal("body decode")
				}
			}
			wav := whisperTestWAV(6)
			a, err := store.Upload(context.Background(), req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(wav)), bytes.NewReader(wav))
			if err != nil {
				t.Fatal(err)
			}
			req.Body.Transcribe.AssetID = a.ID()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var firstSpeech uint64
			_, err = e.transcribe(ctx, req, resolved, func(p runtimejobs.Progress) error {
				if p.Commit != nil && len(p.Commit.Items) > 0 && firstSpeech == 0 {
					firstSpeech = p.Commit.Revision
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			wantRevision := uint64(want.StableRounds)
			for float64(wantRevision)*2 < .9+want.FinalizeDelaySec {
				wantRevision++
			}
			if firstSpeech != wantRevision {
				t.Fatalf("committer ignored options: speech revision %d, want %d", firstSpeech, wantRevision)
			}
		})
	}
}

func TestWhisperRealInferenceHarness(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process contract")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, vad := range []bool{false, true} {
		t.Run(strconv.FormatBool(vad), func(t *testing.T) {
			resolved, _ := whisperTestResolved(t, exe, true)
			dir := t.TempDir()
			manifestPath := filepath.Join(dir, "manifest.json")
			data, err := json.Marshal(resolved.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			binary, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, resolved.Manifest.Entry), binary, 0700); err != nil {
				t.Fatal(err)
			}
			wavPath := filepath.Join(dir, "input.wav")
			if err := os.WriteFile(wavPath, whisperTestWAV(6), 0600); err != nil {
				t.Fatal(err)
			}
			model, vadModel := "vad-off-real.bin", ""
			if vad {
				model, vadModel = "vad-on-real.bin", filepath.Join(dir, "trusted-vad.bin")
			}
			t.Setenv("FENGSHEN_WHISPER_REAL_MANIFEST", manifestPath)
			t.Setenv("FENGSHEN_WHISPER_REAL_MODEL", filepath.Join(dir, model))
			t.Setenv("FENGSHEN_WHISPER_REAL_WAV", wavPath)
			t.Setenv("FENGSHEN_WHISPER_REAL_VAD_MODEL", vadModel)
			TestWhisperRealInference(t)
		})
	}
}

// Explicit real inference requires an independently supplied delivered manifest,
// its component directory, local model and prepared WAV. No path is assumed trusted.
func TestWhisperRealInference(t *testing.T) {
	manifestPath := os.Getenv("FENGSHEN_WHISPER_REAL_MANIFEST")
	if manifestPath == "" {
		t.Skip("explicit real manifest/model/WAV not supplied")
	}
	model, wavPath := os.Getenv("FENGSHEN_WHISPER_REAL_MODEL"), os.Getenv("FENGSHEN_WHISPER_REAL_WAV")
	if model == "" || wavPath == "" {
		t.Fatal("real model and WAV required")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := components.ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	stream := m.ComponentID == "whisper-stream"
	protocol := domain.CenterWhisperBatchProtocol
	if stream {
		protocol = domain.CenterWhisperStreamProtocol
	}
	digest := sha256.Sum256(data)
	vadModel := os.Getenv("FENGSHEN_WHISPER_REAL_VAD_MODEL")
	req := runtimejobs.Request{TaskID: "real-task", SessionID: "real-session", Binding: integration.ExecutionBindingRef{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("b", 64)}, Body: runtimejobs.Body{Component: integration.ExecutionComponentRef{ID: m.ComponentID, Version: m.Version, Protocol: protocol, ManifestSHA256: hex.EncodeToString(digest[:])}, Transcribe: &runtimejobs.TranscribeInput{EpisodeNumber: 1, Stream: stream, VAD: vadModel != ""}}}
	models := []ModelFile{{ModelID: "whisper-asr", Path: model}}
	if vadModel != "" {
		models = append(models, ModelFile{ModelID: "whisper-vad", Path: vadModel})
	}
	var entry runtimeprocess.VerifiedBinary
	for _, file := range m.Files {
		if file.Path == m.Entry {
			entry, err = runtimeprocess.VerifyBinary(filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(m.Entry)), file.SHA256, m.ComponentID+"/"+m.Version, req.Binding.SHA256)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := runtimeassets.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wav, err := os.Open(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	defer wav.Close()
	st, err := wav.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	a, err := store.Upload(ctx, req.TaskID, req.SessionID, runtimeassets.WAV, st.Size(), wav)
	if err != nil {
		t.Fatal(err)
	}
	req.Body.Transcribe.AssetID = a.ID()
	e := &Executor{assets: store, scratch: t.TempDir()}
	defer e.supervisor.Close()
	commits, words := 0, 0
	result, err := e.transcribe(ctx, req, Resolved{Manifest: m, ComponentRoot: filepath.Dir(manifestPath), Entry: entry, Models: models}, func(p runtimejobs.Progress) error {
		if p.Commit != nil {
			commits++
			for _, i := range p.Commit.Items {
				words += len(i.Words)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stream && (result.Stream == nil || !result.Stream.Final || commits == 0 || words == 0) {
		t.Fatal("real stream has no speech commits")
	}
	if !stream && len(result.Items) == 0 {
		t.Fatal("real batch has no speech")
	}
	t.Logf("real inference stream=%v commits=%d words=%d batch_items=%d", stream, commits, words, len(result.Items))
}
