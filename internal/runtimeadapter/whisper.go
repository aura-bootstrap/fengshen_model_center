package runtimeadapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentclient"
	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
)

var errWhisperResult = errors.New("center_result_invalid")

// Retain only an allowlisted native code, never the frame or its cause.
// Private fields keep local diagnostics out of JSON and HTTP errors.
type whisperInferenceError struct {
	code string
}

func (*whisperInferenceError) Error() string { return "center_result_invalid" }

// transcribe accepts only task-owned prepared audio and internally resolved models.
func (e *Executor) transcribe(ctx context.Context, req runtimejobs.Request, resolved Resolved, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	if err := ctx.Err(); err != nil {
		return runtimejobs.Result{}, err
	}
	input := req.Body.Transcribe
	if input == nil || input.EpisodeNumber < 1 || input.OffsetMS < 0 || input.OffsetMS > 86400000 || input.Options != nil && (!input.Stream || input.Options.Validate() != nil) {
		return runtimejobs.Result{}, errors.New("center_request_invalid")
	}
	component, protocol := "whisper-batch", domain.CenterWhisperBatchProtocol
	if input.Stream {
		component, protocol = "whisper-stream", transcript.StreamProtocolVersion
	}
	if resolved.Manifest.ComponentID != component || req.Body.Component.ID != component || resolved.Manifest.Version != req.Body.Component.Version || resolved.Manifest.EntryType != "executable" || resolved.Manifest.Packaging != "native" || components.Validate(resolved.Manifest) != nil {
		return runtimejobs.Result{}, errors.New("center_protocol_unsupported")
	}
	supported := false
	for _, p := range resolved.Manifest.Protocols {
		if p == protocol {
			supported = true
		}
	}
	if !supported || req.Body.Component.Protocol != protocol {
		return runtimejobs.Result{}, errors.New("center_protocol_unsupported")
	}
	// Reconstruct the descriptor from the manifest instead of trusting an unrelated
	// VerifiedBinary supplied alongside it. Start verifies it again before launch.
	var pinned runtimeprocess.VerifiedBinary
	entryPath := ""
	for _, file := range resolved.Manifest.Files {
		if file.Path == resolved.Manifest.Entry {
			entryPath = filepath.Join(resolved.ComponentRoot, filepath.FromSlash(file.Path))
			var err error
			pinned, err = runtimeprocess.VerifyBinary(entryPath, file.SHA256, component+"/"+resolved.Manifest.Version, req.Binding.SHA256)
			if err != nil {
				return runtimejobs.Result{}, errors.New("center_component_invalid")
			}
		}
	}
	if pinned == (runtimeprocess.VerifiedBinary{}) || pinned != resolved.Entry || entryPath == "" {
		return runtimejobs.Result{}, errors.New("center_component_invalid")
	}
	if input.VAD && !integration.WhisperVADSupported(req.Body.Component) {
		return runtimejobs.Result{}, errors.New("center_whisper_vad_unsupported")
	}
	model, vadModel := "", ""
	for _, m := range resolved.Models {
		switch m.ModelID {
		case "whisper-asr":
			if model != "" || !filepath.IsAbs(m.Path) {
				return runtimejobs.Result{}, errors.New("center_binding_stale")
			}
			model = m.Path
		case "whisper-vad":
			if vadModel != "" || !filepath.IsAbs(m.Path) {
				return runtimejobs.Result{}, errors.New("center_binding_stale")
			}
			vadModel = m.Path
		default:
			return runtimejobs.Result{}, errors.New("center_binding_stale")
		}
	}
	if model == "" || input.VAD && vadModel == "" {
		return runtimejobs.Result{}, errors.New("center_binding_missing")
	}
	if e.assets == nil {
		return runtimejobs.Result{}, errors.New("center_asset_forbidden")
	}
	source, meta, err := e.assets.Open(ctx, req.TaskID, input.AssetID)
	if err != nil {
		return runtimejobs.Result{}, err
	}
	defer source.Close()
	if meta.Kind != runtimeassets.WAV || meta.SampleRate != 16000 || meta.Channels != 1 || meta.DurationSeconds <= 0 || meta.DurationSeconds > 7200 || meta.DurationSeconds+float64(input.OffsetMS)/1000 > 86400 {
		return runtimejobs.Result{}, errors.New("center_audio_invalid")
	}
	dir, err := os.MkdirTemp(e.scratch, "whisper-")
	if err != nil {
		return runtimejobs.Result{}, errors.New("center_spool_full")
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input.wav")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return runtimejobs.Result{}, errors.New("center_spool_full")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), &whisperContextReader{ctx: ctx, r: io.LimitReader(source, (1<<30)+1)})
	if err != nil {
		return runtimejobs.Result{}, err
	}
	if strconv.FormatInt(n, 10) != meta.Bytes || n > 1<<30 || hex.EncodeToString(h.Sum(nil)) != meta.SHA256 {
		return runtimejobs.Result{}, errWhisperResult
	}
	pcm, count, err := whisperPCM(f, n)
	if err != nil {
		return runtimejobs.Result{}, err
	}
	duration := float64(count) / 16000
	if math.Abs(duration-meta.DurationSeconds) > 1e-9 {
		return runtimejobs.Result{}, errWhisperResult
	}
	if input.Stream {
		return e.whisperStream(ctx, req, resolved, entryPath, pcm, count, path, meta.SHA256, model, vadModel, emit)
	}
	session, err := e.newShimSession(ctx, "whisper-batch", entryPath, "", "")
	if err != nil {
		return runtimejobs.Result{}, err
	}
	defer session.Close()
	models := map[string][]string{"whisper-asr": {model}}
	if input.VAD {
		models["whisper-vad"] = []string{vadModel}
	}
	if emit != nil {
		if err := emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning}); err != nil {
			return runtimejobs.Result{}, err
		}
	}
	snap, err := session.client.SubmitJob(ctx, componentprotocol.JobRequest{
		Protocol:  componentprotocol.BatchHTTPv1,
		Kind:      "whisper-batch",
		RequestID: req.RequestID,
		Input:     componentprotocol.JobInput{AudioPath: path},
		Models:    models,
		Options:   componentprotocol.JobOptions{VAD: input.VAD},
	})
	if err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, errors.New("center_whisper_failed"))
	}
	snap, err = session.client.WaitJob(ctx, snap.ID)
	if err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = session.client.CancelJob(cancelCtx, snap.ID)
		cancel()
		return runtimejobs.Result{}, whisperContextError(ctx, errors.New("center_whisper_failed"))
	}
	if snap.State != componentprotocol.JobSucceeded {
		return runtimejobs.Result{}, whisperContextError(ctx, errors.New("center_whisper_failed"))
	}
	data, err := session.client.DownloadArtifact(ctx, snap.ID, "transcript.json", runtimejobs.MaxResultBytes)
	if err != nil {
		return runtimejobs.Result{}, errWhisperResult
	}
	items, err := whisperBatchItems(data, *input, duration)
	if err != nil {
		return runtimejobs.Result{}, err
	}
	return runtimejobs.Result{Component: req.Body.Component, Items: items}, nil
}

type whisperContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *whisperContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// The upload validator already checks RIFF integrity; recheck the exact analysis
// format here and stream the data chunk rather than retaining an entire episode.
func whisperPCM(f *os.File, size int64) (io.Reader, int64, error) {
	var header [12]byte
	if _, err := f.ReadAt(header[:], 0); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(header[4:]))+8 != size {
		return nil, 0, errWhisperResult
	}
	valid := false
	for pos := int64(12); pos+8 <= size; {
		var b [8]byte
		if _, err := f.ReadAt(b[:], pos); err != nil {
			return nil, 0, errWhisperResult
		}
		length := int64(binary.LittleEndian.Uint32(b[4:]))
		pos += 8
		if length > size-pos {
			return nil, 0, errWhisperResult
		}
		switch string(b[:4]) {
		case "fmt ":
			var format [16]byte
			if length < 16 {
				return nil, 0, errWhisperResult
			}
			if _, err := f.ReadAt(format[:], pos); err != nil {
				return nil, 0, errWhisperResult
			}
			valid = binary.LittleEndian.Uint16(format[:]) == 1 && binary.LittleEndian.Uint16(format[2:]) == 1 && binary.LittleEndian.Uint32(format[4:]) == 16000 && binary.LittleEndian.Uint32(format[8:]) == 32000 && binary.LittleEndian.Uint16(format[12:]) == 2 && binary.LittleEndian.Uint16(format[14:]) == 16
		case "data":
			if !valid || length <= 0 || length%2 != 0 {
				return nil, 0, errWhisperResult
			}
			return io.NewSectionReader(f, pos, length), length / 2, nil
		}
		pos += length + (length & 1)
	}
	return nil, 0, errWhisperResult
}

type whisperWire struct {
	Protocol  string                  `json:"protocol"`
	Type      string                  `json:"type"`
	RequestID string                  `json:"request_id"`
	Episode   int                     `json:"episode_number"`
	Revision  uint64                  `json:"revision"`
	Through   *float64                `json:"committed_through_sec,omitempty"`
	Items     []domain.TranscriptItem `json:"items,omitempty"`
	Final     bool                    `json:"final,omitempty"`
	ErrorCode string                  `json:"error_code,omitempty"`
}

func whisperReadMessage(reader io.Reader, kind, id string, episode int, revision uint64) (whisperWire, error) {
	// One byte reads are avoided in production by passing a persistent bufio.Reader.
	r, ok := reader.(*bufio.Reader)
	if !ok {
		r = bufio.NewReaderSize(reader, (1<<20)+1)
	}
	line, err := r.ReadSlice('\n')
	var m whisperWire
	if err == nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) != nil {
			return m, errWhisperResult
		}
		allowed := map[string]bool{"protocol": true, "type": true, "request_id": true, "episode_number": true, "revision": true, "committed_through_sec": true, "items": true, "final": true, "error_code": true}
		for key, value := range fields {
			if !allowed[key] || string(value) == "null" {
				return m, errWhisperResult
			}
		}
	}
	if err != nil || len(line) > 1<<20 || !utf8.Valid(line) || components.StrictJSON(line, &m) != nil || m.Protocol != transcript.StreamProtocolVersion || m.RequestID != id || m.Episode != episode || m.Revision != revision {
		return whisperWire{}, errWhisperResult
	}
	if m.Type == "error" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) != nil || len(fields) != 6 || fields["protocol"] == nil || fields["type"] == nil || fields["request_id"] == nil || fields["episode_number"] == nil || fields["revision"] == nil || fields["error_code"] == nil {
			return whisperWire{}, errWhisperResult
		}
		// Decoder failure constants in assets/whisper/stream/main.cpp.
		switch m.ErrorCode {
		case "window_unstable", "window_limit", "model_load", "state_init", "vad_model_load", "cancelled":
			return whisperWire{}, &whisperInferenceError{code: m.ErrorCode}
		default:
			return whisperWire{}, errWhisperResult
		}
	}
	if m.Type != kind || m.ErrorCode != "" {
		return whisperWire{}, errWhisperResult
	}
	if kind == "ready" {
		if m.Through != nil || m.Items != nil || m.Final {
			return m, errWhisperResult
		}
	} else {
		if m.Through == nil || m.Items == nil || (kind == "final") != m.Final {
			return m, errWhisperResult
		}
		// Strict DTO conversion also rejects null/case-folded item fields and bad ranges.
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) != nil {
			return m, errWhisperResult
		}
		raw := []byte(`{"episode_number":` + strconv.Itoa(episode) + `,"revision":` + strconv.FormatUint(revision, 10) + `,"committed_through_sec":` + string(fields["committed_through_sec"]) + `,"items":` + string(fields["items"]) + `,"final":` + strconv.FormatBool(m.Final) + `}`)
		if _, err := runtimejobs.StreamCommitFromJSON(raw, runtimejobs.TranscribeInput{Stream: true, EpisodeNumber: episode}, nil); err != nil {
			return m, errWhisperResult
		}
	}
	return m, nil
}

func whisperWSRead(ctx context.Context, stream *componentclient.Stream, kind, id string, episode int, revision uint64) (whisperWire, error) {
	raw, err := stream.ReadRaw(ctx)
	if err != nil {
		return whisperWire{}, err
	}
	data := bytes.ReplaceAll(raw, []byte(componentprotocol.StreamWSv1), []byte(transcript.StreamProtocolVersion))
	data = append(data, '\n')
	m, readErr := whisperReadMessage(bytes.NewReader(data), kind, id, episode, revision)
	if readErr == nil {
		return m, nil
	}
	var e struct {
		Protocol      string `json:"protocol"`
		Type          string `json:"type"`
		RequestID     string `json:"request_id"`
		EpisodeNumber int    `json:"episode_number"`
		ErrorCode     string `json:"error_code"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Protocol == transcript.StreamProtocolVersion && e.Type == "error" && e.RequestID == id && e.EpisodeNumber == episode {
		switch e.ErrorCode {
		case "window_unstable", "window_limit", "model_load", "state_init", "vad_model_load", "cancelled":
			return whisperWire{}, &whisperInferenceError{code: e.ErrorCode}
		}
	}
	return whisperWire{}, readErr
}

func (e *Executor) whisperStream(ctx context.Context, req runtimejobs.Request, resolved Resolved, entryPath string, pcm io.Reader, count int64, path, digest, model, vadModel string, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	input := *req.Body.Transcribe
	if emit == nil {
		return runtimejobs.Result{}, errors.New("center_request_invalid")
	}
	duration := float64(count) / 16000
	prepared := audio.PreparedAudio{WAVPath: path, SourceVideoPath: path, EpisodeNumber: input.EpisodeNumber, DurationSec: duration, MediaDurationSec: duration, SampleCount: count, Format: audio.AnalysisFormat(), ContentSHA256: digest}
	options := transcript.DefaultStreamOptions()
	if input.Options != nil {
		if input.Options.Validate() != nil {
			return runtimejobs.Result{}, errors.New("center_request_invalid")
		}
		options = transcript.StreamOptions(*input.Options)
	}
	committer, err := transcript.NewStablePrefixCommitter(domain.Episode{EpisodeNumber: input.EpisodeNumber, VideoPath: path}, prepared, "center-whisper-stream", options, nil)
	if err != nil {
		return runtimejobs.Result{}, errWhisperResult
	}
	chunks := (count + 31999) / 32000
	session, err := e.newShimSession(ctx, "whisper-stream", entryPath, "", "")
	if err != nil {
		return runtimejobs.Result{}, err
	}
	defer session.Close()
	stream, err := session.client.DialStream(ctx)
	if err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, errors.New("center_whisper_failed"))
	}
	defer stream.Close()
	id := "E" + strconv.Itoa(input.EpisodeNumber) + "-" + digest[:16]
	models := map[string]string{"whisper-asr": model}
	if input.VAD {
		models["whisper-vad"] = vadModel
	}
	if err := stream.Send(ctx, map[string]any{"protocol": componentprotocol.StreamWSv1, "type": "start", "request_id": id, "episode_number": input.EpisodeNumber, "duration_sec": duration, "format": "s16le/16000/1", "options": options, "models": models, "vad": input.VAD}); err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, err)
	}
	if _, err := whisperWSRead(ctx, stream, "ready", id, input.EpisodeNumber, 0); err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, err)
	}
	var previous *runtimejobs.StreamCommit
	committedItems := make([]runtimejobs.TranscriptItem, 0)
	publish := func(m whisperWire) error {
		commit, err := committer.Push(transcript.StreamHypothesis{EpisodeNumber: input.EpisodeNumber, Revision: m.Revision, CommittedThroughSec: *m.Through, Items: m.Items, Final: m.Final})
		if err != nil {
			return errWhisperResult
		}
		if commit == nil {
			return nil
		}
		projected, err := whisperCommit(*commit, input, previous)
		if err != nil {
			return err
		}
		previous = &projected
		raw, _ := json.Marshal(projected)
		var event runtimejobs.StreamCommit
		_ = json.Unmarshal(raw, &event)
		if err := emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning, Completed: int(m.Revision), Total: int(chunks + 1), Commit: &event}); err != nil {
			return err
		}
		committedItems = append(committedItems, projected.Items...)
		return nil
	}
	var revision uint64 = 1
	buffer := make([]byte, 64000)
	for offset := int64(0); offset < count; revision++ {
		if err := ctx.Err(); err != nil {
			return runtimejobs.Result{}, err
		}
		samples := int64(32000)
		if count-offset < samples {
			samples = count - offset
		}
		if _, err := io.ReadFull(pcm, buffer[:samples*2]); err != nil {
			return runtimejobs.Result{}, errWhisperResult
		}
		end := offset + samples
		if err := stream.Send(ctx, map[string]any{"protocol": componentprotocol.StreamWSv1, "type": "audio", "request_id": id, "episode_number": input.EpisodeNumber, "revision": revision, "start_sec": float64(offset) / 16000, "end_sec": float64(end) / 16000, "sample_count": samples, "pcm": base64.StdEncoding.EncodeToString(buffer[:samples*2])}); err != nil {
			return runtimejobs.Result{}, whisperContextError(ctx, err)
		}
		m, err := whisperWSRead(ctx, stream, "hypothesis", id, input.EpisodeNumber, revision)
		if err != nil {
			return runtimejobs.Result{}, whisperContextError(ctx, err)
		}
		if math.Abs(*m.Through-float64(end)/16000) > 1e-9 {
			return runtimejobs.Result{}, errWhisperResult
		}
		if err := publish(m); err != nil {
			return runtimejobs.Result{}, err
		}
		offset = end
	}
	if err := stream.Send(ctx, map[string]any{"protocol": componentprotocol.StreamWSv1, "type": "finalize", "request_id": id, "episode_number": input.EpisodeNumber, "revision": revision, "committed_through_sec": duration}); err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, err)
	}
	m, err := whisperWSRead(ctx, stream, "final", id, input.EpisodeNumber, revision)
	if err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, err)
	}
	if math.Abs(*m.Through-duration) > 1e-9 {
		return runtimejobs.Result{}, errWhisperResult
	}
	if err := stream.ExpectNormalClose(ctx); err != nil {
		return runtimejobs.Result{}, whisperContextError(ctx, errors.New("center_whisper_failed"))
	}
	if err := publish(m); err != nil {
		return runtimejobs.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return runtimejobs.Result{}, err
	}
	if previous == nil || !previous.Final {
		return runtimejobs.Result{}, errWhisperResult
	}
	final := *previous
	final.Items = committedItems
	return runtimejobs.Result{Component: req.Body.Component, Stream: &final}, nil
}

func whisperContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Received-audio time is not a safe stable watermark: pending words can start
// before it. Until final, publish only the end of the actual immutable prefix.
func whisperCommit(c transcript.StreamCommit, input runtimejobs.TranscribeInput, previous *runtimejobs.StreamCommit) (runtimejobs.StreamCommit, error) {
	offset := float64(input.OffsetMS) / 1000
	through := offset
	if previous != nil {
		through = previous.CommittedThroughSec
	}
	if c.Final {
		through = c.CommittedThroughSec + offset
	} else if len(c.Items) > 0 {
		through = c.Items[len(c.Items)-1].EndSec + offset
	}
	projected := runtimejobs.StreamCommit{EpisodeNumber: c.EpisodeNumber, Revision: c.Revision, CommittedThroughSec: through, Final: c.Final, Items: make([]runtimejobs.TranscriptItem, len(c.Items))}
	for i, item := range c.Items {
		v := runtimejobs.TranscriptItem{StartSec: item.StartSec + offset, EndSec: item.EndSec + offset, Text: item.Text, Probability: item.Probability, Words: make([]runtimejobs.TranscriptWord, len(item.Words))}
		for j, w := range item.Words {
			v.Words[j] = runtimejobs.TranscriptWord{StartSec: w.StartSec + offset, EndSec: w.EndSec + offset, Text: w.Text, Probability: w.Probability}
		}
		projected.Items[i] = v
	}
	raw, err := json.Marshal(projected)
	if err != nil {
		return runtimejobs.StreamCommit{}, errWhisperResult
	}
	return runtimejobs.StreamCommitFromJSON(raw, input, previous)
}

type whisperOffsets struct {
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}
type whisperTimestamps struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type whisperBatchToken struct {
	Offsets    *whisperOffsets    `json:"offsets,omitempty"`
	Timestamps *whisperTimestamps `json:"timestamps,omitempty"`
	Text       string             `json:"text"`
	ID         int                `json:"id"`
	P          *float64           `json:"p,omitempty"`
	DTW        *float64           `json:"t_dtw,omitempty"`
}
type whisperBatchSegment struct {
	Offsets    whisperOffsets      `json:"offsets"`
	Timestamps *whisperTimestamps  `json:"timestamps,omitempty"`
	Text       string              `json:"text"`
	Tokens     []whisperBatchToken `json:"tokens"`
}
type whisperModelDimensions struct {
	Ctx   int `json:"ctx"`
	State int `json:"state"`
	Head  int `json:"head"`
	Layer int `json:"layer"`
}
type whisperBatchDocument struct {
	System string `json:"systeminfo,omitempty"`
	Model  *struct {
		Type         string                 `json:"type"`
		Multilingual bool                   `json:"multilingual"`
		Vocab        int                    `json:"vocab"`
		Audio        whisperModelDimensions `json:"audio"`
		Text         whisperModelDimensions `json:"text"`
		Mels         int                    `json:"mels"`
		Ftype        int                    `json:"ftype"`
	} `json:"model,omitempty"`
	Params *struct {
		Model     string `json:"model"`
		Language  string `json:"language"`
		Translate bool   `json:"translate"`
	} `json:"params,omitempty"`
	Result *struct {
		Language string `json:"language"`
	} `json:"result,omitempty"`
	Transcription []whisperBatchSegment `json:"transcription"`
}

// encoding/json otherwise accepts folded keys and null numeric values.
func whisperExactJSON(data []byte, typ reflect.Type) bool {
	if strings.TrimSpace(string(data)) == "null" {
		return false
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return false
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		for k, v := range obj {
			t, ok := fields[k]
			if !ok || !whisperExactJSON(v, t) {
				return false
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return false
		}
		for _, v := range values {
			if !whisperExactJSON(v, typ.Elem()) {
				return false
			}
		}
	}
	return true
}

func whisperBatchItems(data []byte, input runtimejobs.TranscribeInput, duration float64) ([]runtimejobs.TranscriptItem, error) {
	var doc whisperBatchDocument
	if len(data) > runtimejobs.MaxResultBytes || !utf8.Valid(data) || components.StrictJSON(data, &doc) != nil || !whisperExactJSON(data, reflect.TypeOf(doc)) || doc.Transcription == nil {
		return nil, errWhisperResult
	}
	items := make([]domain.TranscriptItem, 0, len(doc.Transcription))
	for _, s := range doc.Transcription {
		if s.Offsets.From == nil || s.Offsets.To == nil || len(s.Tokens) == 0 {
			return nil, errWhisperResult
		}
		item := domain.TranscriptItem{StartSec: float64(*s.Offsets.From) / 1000, EndSec: float64(*s.Offsets.To) / 1000, Text: strings.TrimSpace(s.Text), Words: make([]domain.TranscriptWord, 0, len(s.Tokens))}
		var joined strings.Builder
		for _, t := range s.Tokens {
			text := strings.TrimSpace(t.Text)
			if (strings.HasPrefix(text, "<|") && strings.HasSuffix(text, "|>")) || (strings.HasPrefix(text, "[_") && strings.HasSuffix(text, "]")) || text == "" {
				continue
			}
			if t.Offsets == nil || t.Offsets.From == nil || t.Offsets.To == nil {
				return nil, errWhisperResult
			}
			item.Words = append(item.Words, domain.TranscriptWord{StartSec: float64(*t.Offsets.From) / 1000, EndSec: float64(*t.Offsets.To) / 1000, Text: t.Text, Probability: t.P})
			joined.WriteString(t.Text)
		}
		if len(item.Words) == 0 || strings.Join(strings.Fields(joined.String()), "") != strings.Join(strings.Fields(item.Text), "") {
			return nil, errWhisperResult
		}
		items = append(items, item)
	}
	// Reuse the same strict word, probability, ordering and media-bound checks as stream.
	streamInput := input
	streamInput.Stream = true
	c, err := whisperCommit(transcript.StreamCommit{EpisodeNumber: input.EpisodeNumber, Revision: 1, CommittedThroughSec: duration, Final: true, Items: items}, streamInput, nil)
	if err != nil || len(c.Items) == 0 {
		return nil, errWhisperResult
	}
	return c.Items, nil
}
