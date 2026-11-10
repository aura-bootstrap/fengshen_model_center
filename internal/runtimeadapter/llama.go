package runtimeadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
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
	session    *shimSession
	identity   string
	cancel     context.CancelFunc
	release    func()
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
	if r.session != nil {
		_ = r.session.Close()
		r.session = nil
	}
	if r.release != nil {
		release := r.release
		r.release = nil
		release()
	}
	r.identity = ""
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

func llamaBindingPlan(r Resolved, vision bool, avx2 func() bool) (runtimeprocess.VerifiedBinary, string, string, error) {
	m := r.Manifest
	declared := false
	for _, protocol := range m.Protocols {
		declared = declared || protocol == domain.CenterLlamaProtocol
	}
	if r.Binding.Component.Protocol != domain.CenterLlamaProtocol || !declared {
		return runtimeprocess.VerifiedBinary{}, "", "", errors.New("center_protocol_unsupported")
	}
	if components.Validate(m) != nil || m.ComponentID != "llama-runtime" || !llamaVersionCompatible(m.Version, m.Version == "b12182-fengshen2-cpu-avx2" && avx2()) || m.OS != "windows" || m.Arch != "amd64" || m.Packaging != "native" || r.Binding.Device != "cpu" || r.Binding.Component.ID != m.ComponentID || r.Binding.Component.Version != m.Version {
		return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
	}
	locked := false
	for _, b := range m.BuildInputs {
		if b.Version == "fd27b2034905f8dadd8bd6d2cc44595f4929d391" && b.SHA256 == "c85cfdb95652f40b9014e83cf2d0b9c406c848a495de3fafc37b935581f7040d" {
			locked = true
		}
	}
	if !locked {
		return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
	}
	id := "local-text"
	if vision {
		id = "local-vision"
	}
	var models, projectors []string
	for _, f := range r.Models {
		if f.ModelID != id {
			return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
		}
		if vision && strings.HasPrefix(strings.ToLower(filepath.Base(f.Path)), "mmproj-") {
			projectors = append(projectors, f.Path)
		} else {
			models = append(models, f.Path)
		}
	}
	projector := ""
	if vision {
		if len(models) != 1 || len(projectors) != 1 || filepath.Dir(models[0]) != filepath.Dir(projectors[0]) {
			return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
		}
		switch filepath.Base(models[0]) {
		case "SmolVLM-500M-Instruct-Q8_0.gguf":
			projector = "mmproj-SmolVLM-500M-Instruct-f16.gguf"
		case "Qwen3-VL-2B-Instruct-Q8_0.gguf":
			projector = "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf"
		}
		if projector == "" || filepath.Base(projectors[0]) != projector {
			return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
		}
		projector = projectors[0]
		if !filepath.IsAbs(projector) || strings.ToLower(filepath.Ext(projector)) != ".gguf" {
			return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
		}
	} else if len(projectors) != 0 {
		return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
	}
	model, err := llamaModel(models)
	if err != nil {
		return runtimeprocess.VerifiedBinary{}, "", "", err
	}
	raw, err := json.Marshal(r.Binding)
	if err != nil {
		return runtimeprocess.VerifiedBinary{}, "", "", errLlamaBinding
	}
	sum := sha256.Sum256(raw)
	for _, f := range m.Files {
		if f.Path == "internal/llama-server.exe" {
			b, err := runtimeprocess.VerifyBinary(filepath.Join(r.ComponentRoot, filepath.FromSlash(f.Path)), f.SHA256, m.ComponentID+"/"+m.Version, hex.EncodeToString(sum[:]))
			return b, model, projector, err
		}
	}
	return runtimeprocess.VerifiedBinary{}, "", "", errors.New("center_component_missing")
}

// llamaShimPlan keeps all binding validation in the center and returns only the
// typed start request; the shim owns the fixed llama-server arguments.
func llamaShimPlan(r Resolved, vision bool) (string, componentprotocol.LlamaStartRequest, error) {
	b, model, projector, err := llamaBindingPlan(r, vision, llamaHostAVX2)
	if err != nil {
		return "", componentprotocol.LlamaStartRequest{}, err
	}
	return b.Path(), componentprotocol.LlamaStartRequest{Protocol: componentprotocol.LlamaHTTPv1, Model: model, MMProj: projector, Vision: vision}, nil
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
	target, startReq, err := llamaShimPlan(resolved, vision)
	if err != nil {
		return fail(err)
	}
	identity, err := llamaCacheIdentity(resolved)
	if err != nil {
		return fail(err)
	}
	if state.session != nil && state.identity != identity {
		state.unload()
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageLoading}); err != nil {
			return fail(err)
		}
	}
	if state.session == nil {
		life, stop := context.WithCancel(e.lifetime)
		session, err := e.newShimSession(life, "llama", target, "", "")
		if err != nil {
			stop()
			return fail(err)
		}
		startCtx, cancelStart := context.WithTimeout(runCtx, 3*time.Minute)
		err = session.client.StartLlama(startCtx, startReq)
		cancelStart()
		if err != nil {
			_ = session.Close()
			stop()
			return fail(errors.New("center_llama_load_failed"))
		}
		state.session = session
		state.cancel = stop
		state.identity = identity
		state.release = release
		release = nil
	} else if release != nil {
		release()
		release = nil
	}
	client := state.session.client
	// Health and inference both traverse the shim; the upstream key stays inside it.
	if err = client.LlamaHealth(runCtx); err != nil {
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
	base, key := client.BaseURL(), client.Token()
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
