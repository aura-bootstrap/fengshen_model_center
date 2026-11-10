package runtimeadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
	"github.com/aura-bootstrap/fengshen_model_center/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeprocess"
)

var errLlamaBinding = errors.New("center_model_incompatible")

// Local diagnostics retain only allowlisted enums and numbers, never the cause.
// Keep fields private so JSON and the HTTP error boundary remain unchanged.
type llamaInferenceError struct {
	kind                   scoring.ErrorKind
	code                   string
	statusCode             int
	attempts               int
	parseCategory          scoring.ParseCategory
	finishReason           scoring.FinishReason
	contentLength          int
	eventCategory          scoring.EventCategory
	eventCount             int
	invalidEventCount      int
	firstInvalidEventIndex int
	scoreField             scoring.ScoreField
	scoreReason            scoring.ScoreReason
}

func (*llamaInferenceError) Error() string { return "center_llama_inference_failed" }

func llamaInferenceFailure(err error) *llamaInferenceError {
	diagnostic := &llamaInferenceError{}
	var classified *scoring.Error
	if !errors.As(err, &classified) || classified == nil {
		return diagnostic
	}
	switch classified.Kind {
	case scoring.ErrorKindConfig, scoring.ErrorKindRequest, scoring.ErrorKindTransport,
		scoring.ErrorKindTimeout, scoring.ErrorKindCanceled, scoring.ErrorKindHTTP,
		scoring.ErrorKindProtocol, scoring.ErrorKindJSON, scoring.ErrorKindUnauthorized,
		scoring.ErrorKindQuota, scoring.ErrorKindAudioAnalysis, scoring.ErrorKindTranscript,
		scoring.ErrorKindLocalModelConfig, scoring.ErrorKindLocalEndpoint, scoring.ErrorKindLocalSecret,
		scoring.ErrorKindLocalModel, scoring.ErrorKindAudioFeatures, scoring.ErrorKindAnalysisChain,
		scoring.ErrorKindAmbiguousExecution, scoring.ErrorKindLocalVision:
		diagnostic.kind = classified.Kind
	}
	switch classified.Code {
	case scoring.CodeConfigInvalid, scoring.CodeRequestInvalid, scoring.CodeUnauthorized,
		scoring.CodeQuotaExceeded, scoring.CodeProviderTimeout, scoring.CodeProviderUnavailable,
		scoring.CodeResponseInvalid, scoring.CodeCanceled, scoring.CodeAudioAnalysisFailed,
		scoring.CodeTranscriptFailed, scoring.CodeLocalModelConfigInvalid, scoring.CodeLocalEndpointRejected,
		scoring.CodeLocalSecretUnavailable, scoring.CodeLocalModelUnavailable, scoring.CodeAudioFeaturesInvalid,
		scoring.CodeAnalysisChainInvalid, scoring.CodeAmbiguousExecution, scoring.CodeLocalVisionFailed:
		diagnostic.code = classified.Code
	}
	diagnostic.statusCode = classified.StatusCode
	diagnostic.attempts = classified.Attempts
	diagnostic.parseCategory = classified.ParseCategory()
	diagnostic.finishReason = classified.FinishReason()
	diagnostic.contentLength = classified.ContentLength()
	switch category := classified.EventCategory(); category {
	case scoring.EventObject, scoring.EventDecode, scoring.EventScore, scoring.EventTitle,
		scoring.EventReason, scoring.EventTags, scoring.EventBool,
		scoring.EventRecommendedPair, scoring.EventRecommendedRange:
		diagnostic.eventCategory = category
	}
	diagnostic.eventCount = classified.EventCount()
	diagnostic.invalidEventCount = classified.InvalidEventCount()
	diagnostic.firstInvalidEventIndex = classified.FirstInvalidEventIndex()
	if diagnostic.eventCategory == scoring.EventScore {
		switch field := classified.ScoreField(); field {
		case scoring.ScoreFieldHighlight, scoring.ScoreFieldConflict, scoring.ScoreFieldEmotion,
			scoring.ScoreFieldReversal, scoring.ScoreFieldVisual, scoring.ScoreFieldHook, scoring.ScoreFieldCompleteness:
			diagnostic.scoreField = field
		}
		switch reason := classified.ScoreReason(); reason {
		case scoring.ScoreReasonMissing, scoring.ScoreReasonNull, scoring.ScoreReasonOutOfRange:
			diagnostic.scoreReason = reason
		}
	}
	return diagnostic
}

type llamaResultError struct {
	eventCategory     string
	eventCount        int
	invalidEventCount int
}

func (*llamaResultError) Error() string { return "center_result_invalid" }

func llamaResultFailure(events []*scoring.RawEvent, input runtimejobs.ScoreInput) *llamaResultError {
	diagnostic := &llamaResultError{eventCategory: "unknown", eventCount: len(events)}
	if events == nil {
		diagnostic.eventCategory = "nil"
		return diagnostic
	}
	if len(events) > 128 {
		diagnostic.eventCategory = "events_limit"
	}
	for _, event := range events {
		category := llamaResultEventCategory(event, input)
		if category != "" {
			if diagnostic.invalidEventCount == 0 && diagnostic.eventCategory == "unknown" {
				diagnostic.eventCategory = category
			}
			diagnostic.invalidEventCount++
		}
	}
	return diagnostic
}

func llamaResultEventCategory(event *scoring.RawEvent, input runtimejobs.ScoreInput) string {
	if event == nil {
		return "nil"
	}
	for _, score := range []float64{event.HighlightScore, event.ConflictScore, event.EmotionScore, event.ReversalScore, event.VisualScore, event.HookScore, event.CompletenessScore} {
		if !(score >= 0 && score <= 1) {
			return "unknown"
		}
	}
	textValid := func(text string) bool {
		return utf8.ValidString(text) && strings.TrimSpace(text) != "" && len(text) <= 8192
	}
	if !textValid(event.Title) || !textValid(event.Reason) {
		return "text"
	}
	if event.Tags == nil || len(event.Tags) > 128 {
		return "tags"
	}
	for _, tag := range event.Tags {
		if !textValid(tag) {
			return "tags"
		}
	}
	if (event.StartUnitID == nil) != (event.EndUnitID == nil) {
		return "id_pair"
	}
	if (event.RecommendedStartSec == nil) != (event.RecommendedEndSec == nil) {
		return "seconds_range"
	}
	if event.StartUnitID != nil {
		start, end := -1, -1
		for i, unit := range input.EligibleUnits {
			if unit.ID == *event.StartUnitID {
				start = i
			}
			if unit.ID == *event.EndUnitID {
				end = i
			}
		}
		if start < 0 || end < 0 {
			return "id_not_eligible"
		}
		if end < start {
			return "id_reverse"
		}
	}
	if event.RecommendedStartSec != nil {
		start, end := *event.RecommendedStartSec, *event.RecommendedEndSec
		if !(start >= 0 && end > start && end <= 86400) {
			return "seconds_range"
		}
		if start < input.StartSec || end > input.EndSec {
			return "outside_candidate"
		}
	}
	return ""
}

// All cache fields are protected by Executor.heavy, including timer callbacks.
type llamaRuntime struct {
	process    *runtimeprocess.Process
	binary     runtimeprocess.VerifiedBinary
	identity   string
	base, key  string
	cancel     context.CancelFunc
	release    func()
	drains     sync.WaitGroup
	generation uint64
	idleCancel chan struct{}
}

func (r *llamaRuntime) stopIdle() {
	r.generation++
	if r.idleCancel != nil {
		close(r.idleCancel)
		r.idleCancel = nil
	}
}

func (r *llamaRuntime) unload() {
	r.stopIdle()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.process != nil {
		_ = r.process.Close()
		r.process = nil
	}
	r.drains.Wait()
	if r.release != nil {
		release := r.release
		r.release = nil
		release()
	}
	r.binary = runtimeprocess.VerifiedBinary{}
	r.identity = ""
	r.base = ""
	r.key = ""
}

func (e *Executor) idleLlama() {
	r := e.llama
	r.stopIdle()
	generation := r.generation
	stop := make(chan struct{})
	r.idleCancel = stop
	var tick <-chan time.Time
	var timer *time.Timer
	if e.llamaAfter != nil {
		tick = e.llamaAfter(5 * time.Minute)
	} else {
		timer = time.NewTimer(5 * time.Minute)
		tick = timer.C
	}
	e.timers.Add(1)
	go func() {
		defer e.timers.Done()
		if timer != nil {
			defer timer.Stop()
		}
		select {
		case <-tick:
		case <-stop:
			return
		case <-e.lifetime.Done():
			return
		}
		select {
		case e.heavy <- struct{}{}:
		case <-stop:
			return
		case <-e.lifetime.Done():
			return
		}
		defer func() { <-e.heavy }()
		if r.generation == generation {
			r.unload()
		}
	}()
}

func llamaCacheIdentity(r Resolved) (string, error) {
	raw, err := json.Marshal(struct {
		Binding  bindings.Binding
		Manifest components.Manifest
		Root     string
		Models   []ModelFile
	}{r.Binding, r.Manifest, r.ComponentRoot, r.Models})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

var llamaShard = regexp.MustCompile(`^(.*)-([0-9]{5})-of-([0-9]{5})\.gguf$`)

func llamaModel(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errLlamaBinding
	}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	seen := map[string]bool{}
	var prefix string
	for i, p := range paths {
		if !filepath.IsAbs(p) || strings.ToLower(filepath.Ext(p)) != ".gguf" || seen[strings.ToLower(p)] {
			return "", errLlamaBinding
		}
		seen[strings.ToLower(p)] = true
		m := llamaShard.FindStringSubmatch(p)
		if m == nil {
			if len(paths) == 1 {
				return p, nil
			}
			return "", errLlamaBinding
		}
		n, _ := strconv.Atoi(m[2])
		total, _ := strconv.Atoi(m[3])
		if n != i+1 || total != len(paths) || i > 0 && m[1] != prefix {
			return "", errLlamaBinding
		}
		prefix = m[1]
	}
	return paths[0], nil
}

func llamaPlan(r Resolved, vision bool) (runtimeprocess.VerifiedBinary, []string, error) {
	return llamaPlanWithCPU(r, vision, llamaHostAVX2)
}

func llamaPlanWithCPU(r Resolved, vision bool, avx2 func() bool) (runtimeprocess.VerifiedBinary, []string, error) {
	m := r.Manifest
	declared := false
	for _, protocol := range m.Protocols {
		declared = declared || protocol == domain.CenterLlamaProtocol
	}
	if r.Binding.Component.Protocol != domain.CenterLlamaProtocol || !declared {
		return runtimeprocess.VerifiedBinary{}, nil, errors.New("center_protocol_unsupported")
	}
	if components.Validate(m) != nil || m.ComponentID != "llama-runtime" || !llamaVersionCompatible(m.Version, m.Version == "b12182-fengshen2-cpu-avx2" && avx2()) || m.OS != "windows" || m.Arch != "amd64" || m.Packaging != "native" || r.Binding.Device != "cpu" || r.Binding.Component.ID != m.ComponentID || r.Binding.Component.Version != m.Version {
		return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
	}
	locked := false
	for _, b := range m.BuildInputs {
		if b.Version == "fd27b2034905f8dadd8bd6d2cc44595f4929d391" && b.SHA256 == "c85cfdb95652f40b9014e83cf2d0b9c406c848a495de3fafc37b935581f7040d" {
			locked = true
		}
	}
	if !locked {
		return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
	}
	id := "local-text"
	if vision {
		id = "local-vision"
	}
	var models, projectors []string
	for _, f := range r.Models {
		if f.ModelID != id {
			return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
		}
		if vision && strings.HasPrefix(strings.ToLower(filepath.Base(f.Path)), "mmproj-") {
			projectors = append(projectors, f.Path)
		} else {
			models = append(models, f.Path)
		}
	}
	// The registered vision composition is explicit; do not pair arbitrary GGUFs.
	if vision {
		if len(models) != 1 || len(projectors) != 1 || filepath.Dir(models[0]) != filepath.Dir(projectors[0]) {
			return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
		}
		projector := ""
		switch filepath.Base(models[0]) {
		case "SmolVLM-500M-Instruct-Q8_0.gguf":
			projector = "mmproj-SmolVLM-500M-Instruct-f16.gguf"
		case "Qwen3-VL-2B-Instruct-Q8_0.gguf":
			projector = "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf"
		}
		if projector == "" || filepath.Base(projectors[0]) != projector {
			return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
		}
	}
	model, err := llamaModel(models)
	if err != nil {
		return runtimeprocess.VerifiedBinary{}, nil, err
	}
	if vision && len(projectors) != 1 || !vision && len(projectors) != 0 {
		return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
	}
	args := []string{"--host", "127.0.0.1", "--offline", "--no-webui", "--no-warmup", "--parallel", "1", "--ctx-size", "4096", "--n-gpu-layers", "0", "--threads", "4", "--no-mmproj-offload", "--model", model}
	if vision {
		if !filepath.IsAbs(projectors[0]) || strings.ToLower(filepath.Ext(projectors[0])) != ".gguf" {
			return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
		}
		args = append(args, "--mmproj", projectors[0])
	} else {
		args = append(args, "--no-mmproj")
	}
	raw, err := json.Marshal(r.Binding)
	if err != nil {
		return runtimeprocess.VerifiedBinary{}, nil, errLlamaBinding
	}
	sum := sha256.Sum256(raw)
	for _, f := range m.Files {
		if f.Path == "internal/llama-server.exe" {
			b, err := runtimeprocess.VerifyBinary(filepath.Join(r.ComponentRoot, filepath.FromSlash(f.Path)), f.SHA256, m.ComponentID+"/"+m.Version, hex.EncodeToString(sum[:]))
			return b, args, err
		}
	}
	return runtimeprocess.VerifiedBinary{}, nil, errors.New("center_component_missing")
}

func startLlama(ctx context.Context, s *runtimeprocess.Supervisor, b runtimeprocess.VerifiedBinary, args []string, key string) (*runtimeprocess.Process, error) {
	return s.StartLlama(ctx, b, runtimeprocess.Options{Args: args, MaxStdinBytes: 1, MaxStdoutBytes: 16 << 20, MaxStderrBytes: 16 << 20, BufferBytes: 4 << 20}, key)
}

func llamaHTTP() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func llamaReady(ctx context.Context, p *runtimeprocess.Process, b runtimeprocess.VerifiedBinary, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := llamaHTTP()
	defer c.CloseIdleConnections()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		if err := p.CheckIdentity(b); err != nil {
			return err
		}
		hc, stop := context.WithTimeout(ctx, time.Second)
		req, _ := http.NewRequestWithContext(hc, http.MethodGet, base+"/health", nil)
		resp, err := c.Do(req)
		ready := false
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
			resp.Body.Close()
			var health struct {
				Status string `json:"status"`
			}
			ready = readErr == nil && len(data) <= 4096 && resp.StatusCode == 200 && json.Unmarshal(data, &health) == nil && health.Status == "ok"
		}
		stop()
		if ready {
			return p.CheckIdentity(b)
		}
		select {
		case <-ctx.Done():
			return errors.New("center_llama_load_failed")
		case <-timer.C:
		}
	}
}

func (e *Executor) llamaImage(ctx context.Context, req runtimejobs.Request) (*scoring.ImageInput, error) {
	src, meta, err := e.assets.Open(ctx, req.TaskID, req.Body.Vision.AssetID)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if meta.Kind != runtimeassets.Image {
		return nil, runtimeassets.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(src, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, runtimeassets.ErrInvalid
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 || (format != "jpeg" && format != "png") {
		return nil, runtimeassets.ErrInvalid
	}
	mime := "image/" + format
	return &scoring.ImageInput{MIMEType: mime, DataBase64: base64.StdEncoding.EncodeToString(data)}, nil
}

// score also serializes direct internal callers; Execute already holds this gate.
func (e *Executor) score(ctx context.Context, req runtimejobs.Request, resolved Resolved, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	run, done, err := e.enter(ctx)
	if err != nil {
		if resolved.Release != nil {
			resolved.Release()
		}
		return runtimejobs.Result{}, err
	}
	defer done()
	if err = e.lockHeavy(run); err != nil {
		if resolved.Release != nil {
			resolved.Release()
		}
		return runtimejobs.Result{}, err
	}
	defer func() { <-e.heavy }()
	e.llama.stopIdle()
	return e.scoreLocked(run, req, resolved, emit)
}

func (e *Executor) scoreLocked(ctx context.Context, req runtimejobs.Request, resolved Resolved, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	state := e.llama
	release := resolved.Release
	success := false
	defer func() {
		if !success || ctx.Err() != nil {
			state.unload()
		} else {
			e.idleLlama()
		}
		if release != nil {
			release()
		}
	}()
	fail := func(err error) (runtimejobs.Result, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return runtimejobs.Result{}, err
	}
	vision := req.Binding.Scene == "highlight.vision"
	var input runtimejobs.ScoreInput
	if vision {
		if req.Body.Vision == nil || req.Body.Score != nil {
			return fail(errLlamaBinding)
		}
		input = req.Body.Vision.Candidate
	} else {
		if req.Binding.Scene != "highlight.score" || req.Body.Score == nil || req.Body.Vision != nil {
			return fail(errLlamaBinding)
		}
		input = *req.Body.Score
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fail(err)
	}
	input, err = runtimejobs.ScoreInputFromJSON(raw)
	if err != nil {
		return fail(err)
	}
	var candidate scoring.CandidateInput
	if err = json.Unmarshal(raw, &candidate); err != nil {
		return fail(err)
	}
	if candidate.RequestID != "" && candidate.RequestID != req.RequestID || candidate.TaskID != "" && candidate.TaskID != req.TaskID {
		return fail(errors.New("center_request_invalid"))
	}
	candidate.RequestID = req.RequestID
	candidate.TaskID = req.TaskID
	if vision {
		candidate.Image, err = e.llamaImage(ctx, req)
		if err != nil {
			return fail(err)
		}
	} else {
		if _, err = scoring.BuildTextRequest(candidate, scoring.ModelConfig{Model: "bound-model"}); err != nil {
			return fail(errors.New("center_request_invalid"))
		}
	}
	b, args, err := llamaPlan(resolved, vision)
	if err != nil {
		return fail(err)
	}
	identity, err := llamaCacheIdentity(resolved)
	if err != nil {
		return fail(err)
	}
	if state.process != nil && (state.identity != identity || state.binary != b) {
		state.unload()
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageLoading}); err != nil {
			return fail(err)
		}
	}
	if state.process == nil {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fail(errors.New("center_runtime_busy"))
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if port == 11434 {
			listener.Close()
			return fail(errors.New("center_runtime_busy"))
		}
		keyBytes := make([]byte, 32)
		if _, err = rand.Read(keyBytes); err != nil {
			listener.Close()
			return fail(err)
		}
		key := hex.EncodeToString(keyBytes)
		args = append(args, "--port", strconv.Itoa(port))
		listener.Close()
		life, stop := context.WithCancel(e.lifetime)
		p, err := startLlama(life, &e.supervisor, b, args, key)
		if err != nil {
			stop()
			return fail(err)
		}
		state.process = p
		state.cancel = stop
		state.binary = b
		state.identity = identity
		state.base = fmt.Sprintf("http://127.0.0.1:%d", port)
		state.key = key
		state.release = release
		release = nil
		_ = p.Stdin().Close()
		state.drains.Add(2)
		for _, stream := range []io.Reader{p.Stdout(), p.Stderr()} {
			go func(src io.Reader) { defer state.drains.Done(); _, _ = io.Copy(io.Discard, src) }(stream)
		}
	} else if release != nil {
		release()
		release = nil
	}
	p, base, key := state.process, state.base, state.key
	stopped := make(chan struct{})
	stopRequest := context.AfterFunc(runCtx, func() { _ = p.Close(); close(stopped) })
	defer func() {
		if !stopRequest() {
			<-stopped
		}
	}()
	// Check the actual retained binary identity and health for every request.
	if err = llamaReady(runCtx, p, state.binary, base); err != nil {
		if runCtx.Err() != nil {
			return fail(runCtx.Err())
		}
		return fail(errors.New("center_llama_load_failed"))
	}
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning}); err != nil {
			return fail(err)
		}
	}
	if err = p.CheckIdentity(b); err != nil {
		return fail(err)
	}
	var events []*scoring.RawEvent
	if vision {
		c, ce := scoring.NewConstrainedLocalVisionClient(scoring.LocalVisionConfig{BaseURL: base + "/v1", APIKey: key, Model: "bound-model", TimeoutSec: scoring.MaxTimeoutSec, DisableRetries: true})
		if ce != nil {
			return fail(errors.New("center_request_invalid"))
		}
		events, err = c.ScoreEvents(runCtx, candidate)
	} else {
		c, ce := scoring.NewClient(scoring.Config{Provider: scoring.ProviderLocal, BaseURL: base + "/v1", APIKey: key, Model: "bound-model", TimeoutSec: scoring.MaxTimeoutSec, DisableRetries: true})
		if ce != nil {
			return fail(errors.New("center_request_invalid"))
		}
		events, err = c.ScoreEvents(runCtx, candidate)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if err != nil {
		return fail(llamaInferenceFailure(err))
	}
	if err = p.CheckIdentity(b); err != nil {
		return fail(err)
	}
	raw, err = json.Marshal(events)
	if err != nil {
		return fail(llamaResultFailure(events, input))
	}
	converted, err := runtimejobs.RawEventsFromJSON(raw, input)
	if err != nil || converted == nil {
		return fail(llamaResultFailure(events, input))
	}
	if err = runCtx.Err(); err != nil {
		return fail(err)
	}
	success = true
	return runtimejobs.Result{Component: req.Body.Component, Events: converted}, nil
}
