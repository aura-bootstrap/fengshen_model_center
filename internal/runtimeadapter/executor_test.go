package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
)

// This executable is a lifecycle/protocol fixture, never real model inference.
func llamaCacheExecutor(t *testing.T) (*Executor, runtimejobs.Request, *Resolved, *atomic.Int32) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows owned process lifecycle fixture")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.Mkdir(filepath.Join(root, "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "internal", "llama-server.exe")
	if err = os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex(data)
	component := integration.ExecutionComponentRef{ID: "llama-runtime", Version: "b12182-fengshen1-cpu", Protocol: "llama.cpp.server.http/b12182"}
	candidate := scoring.ContractCandidate()
	for i := range candidate.EligibleUnits {
		candidate.EligibleUnits[i].Words = []domain.TranscriptWord{}
	}
	candidate.AnalysisChain = domain.AnalysisChainAudioTextV1
	candidate.AudioSummary = ""
	candidate.ShotSummary = ""
	candidate.AudioFeatures = &domain.CandidateAudioFeatures{Schema: domain.AudioFeatureSchemaV1, Window: domain.AudioInterval{StartSec: 0, EndSec: 60}, VAD: domain.VADStatus{Backend: "energy_v1", Reliable: true}, Loudness: domain.LoudnessMetrics{Reason: "insufficient_voiced_frames"}, SpeechRate: domain.SpeechRateMetrics{Reason: "insufficient_voiced_frames"}, Pitch: domain.PitchMetrics{Reason: "insufficient_voiced_frames"}}
	candidate.AudioFeatures.Speech = []domain.AudioInterval{}
	candidate.AudioFeatures.Silence = []domain.AudioInterval{}
	candidate.AudioFeatures.Pauses = []domain.AudioInterval{}
	candidate.AudioFeatures.Overlaps = []domain.OverlapInterval{}
	candidate.AudioFeatures.UnitMetrics = []domain.UnitAudioMetrics{}
	candidate.AudioFeatures.SoundEvents = []domain.SoundEvent{}
	if _, err := scoring.BuildTextRequest(candidate, scoring.ModelConfig{Model: "bound-model"}); err != nil {
		t.Fatal("invalid test candidate", err)
	}
	raw, _ := json.Marshal(candidate)
	input, err := runtimejobs.ScoreInputFromJSON(raw)
	if err != nil {
		t.Fatalf("input %s: %v", raw, err)
	}
	req := runtimejobs.Request{TaskID: input.TaskID, RequestID: input.RequestID, Binding: integration.ExecutionBindingRef{Scene: "highlight.score", Revision: 1, SHA256: strings.Repeat("a", 64)}, Body: runtimejobs.Body{Component: component, Score: &input}}
	resolved := &Resolved{Binding: bindings.Binding{Scene: req.Binding.Scene, Ref: req.Binding, Component: component, Device: "cpu", Files: []bindings.FrozenFile{{Identity: "model-identity"}}}, ComponentRoot: root, Models: []ModelFile{{ModelID: "local-text", Path: filepath.Join(root, "unloaded.gguf")}}, Manifest: components.Manifest{Schema: 1, ComponentID: component.ID, Version: component.Version, OS: "windows", Arch: "amd64", Packaging: "native", EntryType: "executable", Entry: "internal/llama-server.exe", Protocols: []string{"llama.cpp.server.http/b12182"}, Files: []components.File{{Path: "internal/llama-server.exe", Bytes: strconv.Itoa(len(data)), SHA256: sum}}, BuildInputs: []components.BuildInput{{Version: "fd27b2034905f8dadd8bd6d2cc44595f4929d391", SHA256: "c85cfdb95652f40b9014e83cf2d0b9c406c848a495de3fafc37b935581f7040d"}}}}
	if err := os.WriteFile(resolved.Models[0].Path, []byte("lifecycle fixture, not GGUF weights"), 0600); err != nil {
		t.Fatal(err)
	}
	releases := &atomic.Int32{}
	store, err := runtimeassets.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e, err := New(func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) (Resolved, error) {
		r := *resolved
		lease, err := runtimeprocess.PinFile(r.Models[0].Path)
		if err != nil {
			return Resolved{}, err
		}
		r.Release = func() {
			if e := lease.Close(); e != nil {
				t.Error("lease released twice", e)
			}
			releases.Add(1)
		}
		return r, nil
	}, store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.launchShim = inProcessShimLauncher(t)
	roundtrip, _ := json.Marshal(input)
	var check scoring.CandidateInput
	_ = json.Unmarshal(roundtrip, &check)
	if _, err := runtimejobs.ScoreInputFromJSON(roundtrip); err != nil {
		t.Fatalf("second validation %s: %v", roundtrip, err)
	}
	if _, err := scoring.BuildTextRequest(check, scoring.ModelConfig{Model: "bound-model"}); err != nil {
		t.Fatal("roundtrip", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, req, resolved, releases
}

func TestExecutorLlamaRetainsLeaseAndReusesOwnedProcess(t *testing.T) {
	e, req, resolved, releases := llamaCacheExecutor(t)
	var first *shimSession
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 2; i++ {
		callCtx := context.Background()
		if i == 0 {
			callCtx = ctx
		}
		if _, err := e.Execute(callCtx, req, nil); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = e.llama.session
			cancel()
		} else if first != e.llama.session {
			t.Fatal("cache hit started another session")
		}
		if first == nil || first.client == nil {
			t.Fatal("cache did not retain a session")
		}
		if err := os.Rename(resolved.Models[0].Path, resolved.Models[0].Path+".moved"); err == nil {
			t.Fatal("cached process lost pinned model lease")
		}
		if got := releases.Load(); got != int32(i) {
			t.Fatalf("lease released before unload: got %d want %d", got, i)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 2 {
		t.Fatal("cache lease not released exactly once", releases.Load())
	}
	if err := os.Rename(resolved.Models[0].Path, resolved.Models[0].Path+".moved"); err != nil {
		t.Fatal("unload retained model pin", err)
	}
}

func TestExecutorLlamaCancellationAndCrossCapabilityEviction(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	for _, scene := range []string{"highlight.transcribe", "timbre.separate", "timbre.convert"} {
		other := req
		other.Binding.Scene = scene
		// Observe eviction inside resolution, before another adapter can load weights.
		resolve := e.resolve
		e.resolve = func(ctx context.Context, b integration.ExecutionBindingRef, c integration.ExecutionComponentRef) (Resolved, error) {
			if e.llama.session != nil {
				t.Fatal("heavy admission retained llama", scene)
			}
			return resolve(ctx, b, c)
		}
		if _, err := e.Execute(context.Background(), other, nil); err == nil {
			t.Fatal("stale binding accepted")
		}
		e.resolve = resolve
		if scene != "timbre.convert" {
			if _, err := e.Execute(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if releases.Load() != 6 {
		t.Fatal("cross-capability lease count", releases.Load())
	}
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := e.Execute(ctx, req, func(p runtimejobs.Progress) error {
		if p.Stage == runtimejobs.StageRunning {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if releases.Load() != 8 {
		t.Fatal("cancel retained cache lease", releases.Load())
	}
}

func TestExecutorCloseWaitsForResolutionAndRejectsQueued(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	resolve := e.resolve
	e.resolve = func(ctx context.Context, b integration.ExecutionBindingRef, c integration.ExecutionComponentRef) (Resolved, error) {
		close(entered)
		<-finish
		return resolve(ctx, b, c)
	}
	returned := make(chan error, 1)
	go func() { _, err := e.Execute(context.Background(), req, nil); returned <- err }()
	<-entered
	var closers sync.WaitGroup
	closers.Add(2)
	closed := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { defer closers.Done(); _ = e.Close() }()
	}
	go func() { closers.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before resolution completed")
	default:
	}
	<-e.lifetime.Done()
	close(finish)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wait and finish")
	}
	if err := <-returned; err == nil {
		t.Fatal("closed execution succeeded")
	}
	if releases.Load() != 1 {
		t.Fatal("resolution lease leak", releases.Load())
	}
	if _, err := e.Execute(context.Background(), req, nil); err == nil {
		t.Fatal("accepted work after Close")
	}
}

func TestExecutorLlamaCacheIdentityAndReplacement(t *testing.T) {
	e, req, r, releases := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	old := e.llama.session
	identity, err := llamaCacheIdentity(*r)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Resolved){
		"binding":   func(v *Resolved) { v.Binding.Ref.Revision++ },
		"device":    func(v *Resolved) { v.Binding.Device = "cuda" },
		"component": func(v *Resolved) { v.Manifest.Version = "changed" },
		"model identity": func(v *Resolved) {
			v.Binding.Files = append([]bindings.FrozenFile(nil), v.Binding.Files...)
			v.Binding.Files[0].Identity = "changed"
		},
		"model path": func(v *Resolved) { v.Models = append([]ModelFile(nil), v.Models...); v.Models[0].Path += "-changed" },
	} {
		v := *r
		mutate(&v)
		got, err := llamaCacheIdentity(v)
		if err != nil || got == identity {
			t.Fatal("identity not matched", name, err)
		}
	}
	req.Binding.Revision++
	r.Binding.Ref = req.Binding
	if _, err := e.Execute(context.Background(), req, func(p runtimejobs.Progress) error {
		if p.Stage == runtimejobs.StageLoading && old != nil && e.llama.session == old {
			t.Fatal("old process still alive at replacement load")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if e.llama.session == old || releases.Load() != 1 {
		t.Fatal("binding replacement did not unload first")
	}
	// A dead owned process must never be replaced merely because its port answers.
	_ = e.llama.session.Close()
	if _, err := e.Execute(context.Background(), req, nil); err == nil {
		t.Fatal("dead process reused")
	}
	if releases.Load() != 3 {
		t.Fatal("identity failure leaked lease", releases.Load())
	}
}

func helperControl(t *testing.T, session *shimSession, path string) {
	t.Helper()
	if session == nil || session.client == nil {
		t.Fatal("missing llama shim session")
	}
	c := llamaHTTP()
	defer c.CloseIdleConnections()
	req, err := http.NewRequest("GET", session.client.BaseURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.client.Token())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestExecutorLlamaActiveCancelDeadlineAndClose(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "close"} {
		t.Run(mode, func(t *testing.T) {
			e, req, _, releases := llamaCacheExecutor(t)
			if _, err := e.Execute(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			old := e.llama.session
			helperControl(t, old, "/fixture/block")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 300*time.Millisecond)
				defer stop()
			}
			running := make(chan struct{})
			returned := make(chan error, 1)
			go func() {
				_, err := e.Execute(ctx, req, func(p runtimejobs.Progress) error {
					if p.Stage == runtimejobs.StageRunning {
						close(running)
					}
					return nil
				})
				returned <- err
			}()
			<-running
			if mode != "deadline" {
				helperControl(t, old, "/fixture/wait-request")
			}
			if mode == "close" {
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
			} else if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-returned:
				if err == nil {
					t.Fatal("blocked request succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request not interrupted")
			}
			if e.llama.session != nil || releases.Load() != 2 {
				t.Fatal("active stop retained process or lease", releases.Load())
			}
		})
	}
}

func TestExecutorHeavyGateAndHaarException(t *testing.T) {
	e, req, _, _ := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	e.heavy <- struct{}{}
	defer func() { <-e.heavy }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.Execute(ctx, req, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("heavy request bypassed gate", err)
	}
	face := req
	face.Binding.Scene = "image.face"
	if _, err := e.Execute(context.Background(), face, nil); err == nil {
		t.Fatal("stale face accepted")
	}
	if e.llama.session == nil {
		t.Fatal("Haar evicted llama")
	}
}

func TestExecutorLlamaHealthCheckedOnHit(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	helperControl(t, e.llama.session, "/fixture/health-fail")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := e.Execute(ctx, req, nil); err == nil || err.Error() != "center_llama_load_failed" {
		t.Fatal("cache hit skipped health", err)
	}
	if e.llama.session != nil || releases.Load() != 2 {
		t.Fatal("unhealthy cache retained lease")
	}
}

func TestExecutorLlamaIdleTimerGeneration(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	var clocks []chan time.Time
	e.llamaAfter = func(d time.Duration) <-chan time.Time {
		if d != 5*time.Minute {
			t.Errorf("idle timeout %s", d)
		}
		tick := make(chan time.Time, 1)
		clocks = append(clocks, tick)
		return tick
	}
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), req, func(p runtimejobs.Progress) error {
		if p.Stage == runtimejobs.StageRunning {
			clocks[0] <- time.Now()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatal("stale timer killed active request", releases.Load())
	}
	clocks[1] <- time.Now()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for releases.Load() != 2 {
		select {
		case <-deadline.C:
			t.Fatal("idle timer did not unload")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 2 {
		t.Fatal("double release", releases.Load())
	}
}
