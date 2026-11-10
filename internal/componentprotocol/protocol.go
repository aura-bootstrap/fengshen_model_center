// Package componentprotocol defines the bounded HTTP/WS contract between the
// model center and its local shim processes. It owns transport-neutral message
// shapes and validation only; it never opens sockets or starts processes.
package componentprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const (
	StreamWSv1   = "fengshen.stream.ws/v1"
	BatchHTTPv1  = "fengshen.batch.http/v1"
	TimbreHTTPv1 = "fengshen.timbre.http/v1"
	HaarHTTPv1   = "fengshen.haar.http/v1"

	MaxControlJSON  = 64 << 10
	MaxWSFrame      = 1 << 20
	MaxImageBytes   = 8 << 20
	MaxArtifactSize = 1 << 30
)

var ErrInvalid = errors.New("component_protocol_invalid")

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code }

func Fail(code string) *Error {
	message := map[string]string{
		"component_auth":         "内部会话已失效",
		"component_request":      "内部请求无效",
		"component_too_large":    "内部请求超出限额",
		"component_not_found":    "内部资源不存在",
		"component_conflict":     "内部会话忙碌",
		"component_backpressure": "内部背压超限",
		"component_failed":       "内部执行失败",
		"component_cancelled":    "内部执行已取消",
	}[code]
	if message == "" {
		message = "内部服务错误"
	}
	return &Error{Code: code, Message: message}
}

// StrictJSON rejects duplicate keys at any depth, unknown fields and trailing
// content. Transport bodies are bounded by the caller before decoding.
func StrictJSON(data []byte, out any) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty body", ErrInvalid)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := duplicateKeys(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing content", ErrInvalid)
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func duplicateKeys(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return fmt.Errorf("%w: duplicate key", ErrInvalid)
			}
			seen[key] = true
			if err := duplicateKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := duplicateKeys(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unexpected delimiter", ErrInvalid)
	}
	_, err = d.Token()
	return err
}

// StreamMessage is the single text-frame schema shared with the legacy whisper
// NDJSON protocol. Unknown fields are rejected on decode.
type StreamMessage struct {
	Protocol            string            `json:"protocol"`
	Type                string            `json:"type"`
	RequestID           string            `json:"request_id"`
	EpisodeNumber       int               `json:"episode_number"`
	Revision            uint64            `json:"revision,omitempty"`
	CommittedThroughSec *float64          `json:"committed_through_sec,omitempty"`
	DurationSec         *float64          `json:"duration_sec,omitempty"`
	Format              string            `json:"format,omitempty"`
	Options             json.RawMessage   `json:"options,omitempty"`
	Models              map[string]string `json:"models,omitempty"`
	VAD                 bool              `json:"vad,omitempty"`
	StartSec            *float64          `json:"start_sec,omitempty"`
	EndSec              *float64          `json:"end_sec,omitempty"`
	SampleCount         *int64            `json:"sample_count,omitempty"`
	PCM                 string            `json:"pcm,omitempty"`
	Items               []json.RawMessage `json:"items,omitempty"`
	Final               bool              `json:"final,omitempty"`
	ErrorCode           string            `json:"error_code,omitempty"`
}

func DecodeStream(data []byte) (StreamMessage, error) {
	var m StreamMessage
	if len(data) > MaxWSFrame {
		return m, Fail("component_too_large")
	}
	if err := StrictJSON(data, &m); err != nil {
		return m, Fail("component_request")
	}
	if m.Protocol != StreamWSv1 || m.RequestID == "" || len(m.RequestID) > 128 || m.EpisodeNumber < 1 {
		return m, Fail("component_request")
	}
	switch m.Type {
	case "start":
		if m.DurationSec == nil || *m.DurationSec <= 0 || m.Format != "s16le/16000/1" || !streamModels(m.Models, m.VAD) {
			return m, Fail("component_request")
		}
	case "audio":
		if m.StartSec == nil || m.EndSec == nil || m.SampleCount == nil || *m.SampleCount <= 0 || *m.SampleCount > 32000 || m.PCM == "" {
			return m, Fail("component_request")
		}
	case "finalize":
		if m.CommittedThroughSec == nil {
			return m, Fail("component_request")
		}
	default:
		return m, Fail("component_request")
	}
	return m, nil
}

type JobOptions struct {
	VAD    bool   `json:"vad,omitempty"`
	Device string `json:"device,omitempty"`
	Steps  int    `json:"steps,omitempty"`
	Pitch  int    `json:"pitch,omitempty"`
}

type JobInput struct {
	AudioPath     string `json:"audio_path"`
	ReferencePath string `json:"reference_path,omitempty"`
}

// JobRequest carries typed, already verified local bindings. The shim never
// accepts a free-form argument vector or an arbitrary URL.
type JobRequest struct {
	Protocol  string              `json:"protocol"`
	Kind      string              `json:"kind"`
	RequestID string              `json:"request_id"`
	Input     JobInput            `json:"input"`
	Models    map[string][]string `json:"models"`
	Bag       string              `json:"bag,omitempty"`
	Options   JobOptions          `json:"options"`
}

type JobState string

const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

type JobSnapshot struct {
	ID        string   `json:"id"`
	Seq       uint64   `json:"seq"`
	State     JobState `json:"state"`
	Progress  int      `json:"progress,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Error     *Error   `json:"error,omitempty"`
}

func DecodeJobRequest(data []byte) (JobRequest, error) {
	var req JobRequest
	if len(data) > MaxControlJSON {
		return req, Fail("component_too_large")
	}
	if err := StrictJSON(data, &req); err != nil {
		return req, Fail("component_request")
	}
	if len(req.RequestID) > 128 || !ValidAbsPath(req.Input.AudioPath) {
		return req, Fail("component_request")
	}
	switch req.Kind {
	case "whisper-batch":
		if req.Protocol != BatchHTTPv1 || !whisperBatchModels(req.Models) || req.Input.ReferencePath != "" {
			return req, Fail("component_request")
		}
	case "demucs":
		if req.Protocol != TimbreHTTPv1 || !demucsModels(req.Models) || req.Input.ReferencePath != "" || !ValidAbsPath(req.Bag) || req.Options.Device != "cpu" && req.Options.Device != "cuda" {
			return req, Fail("component_request")
		}
	case "seedvc":
		if req.Protocol != TimbreHTTPv1 || !seedVCModels(req.Models) || req.Bag != "" || !ValidAbsPath(req.Input.ReferencePath) || req.Options.Device != "cpu" && req.Options.Device != "cuda" || req.Options.Pitch < -24 || req.Options.Pitch > 24 || req.Options.Steps != 25 {
			return req, Fail("component_request")
		}
	default:
		return req, Fail("component_request")
	}
	for _, paths := range req.Models {
		for _, p := range paths {
			if !ValidAbsPath(p) {
				return req, Fail("component_request")
			}
		}
	}
	return req, nil
}

func streamModels(m map[string]string, vad bool) bool {
	if len(m) == 0 || len(m) > 2 || !ValidAbsPath(m["whisper-asr"]) {
		return false
	}
	if vad {
		if !ValidAbsPath(m["whisper-vad"]) {
			return false
		}
	} else if _, ok := m["whisper-vad"]; ok {
		return false
	}
	for key := range m {
		if key != "whisper-asr" && key != "whisper-vad" {
			return false
		}
	}
	return true
}

func whisperBatchModels(m map[string][]string) bool {
	if len(m) == 0 || len(m) > 2 {
		return false
	}
	if len(m["whisper-asr"]) != 1 {
		return false
	}
	if len(m) == 2 && len(m["whisper-vad"]) != 1 {
		return false
	}
	return true
}

func demucsModels(m map[string][]string) bool {
	if len(m) != 1 {
		return false
	}
	if paths, ok := m["demucs"]; ok {
		return len(paths) == 1
	}
	if paths, ok := m["demucs-ft"]; ok {
		return len(paths) == 4
	}
	return false
}

func seedVCModels(m map[string][]string) bool {
	return len(m) == 4 && len(m["seed-vc"]) == 2 && len(m["seed-whisper"]) == 1 && len(m["campplus"]) == 1 && len(m["bigvgan"]) == 1
}

func ValidAbsPath(p string) bool {
	if p == "" || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	return filepath.Dir(p) != p
}

func ArtifactAllowed(kind, name string) bool {
	switch kind {
	case "whisper-batch":
		return name == "transcript.json"
	case "demucs":
		return name == "vocals.wav" || name == "accompaniment.wav"
	case "seedvc":
		return name == "converted.wav"
	}
	return false
}

func Artifacts(kind string) []string {
	switch kind {
	case "whisper-batch":
		return []string{"transcript.json"}
	case "demucs":
		return []string{"vocals.wav", "accompaniment.wav"}
	case "seedvc":
		return []string{"converted.wav"}
	}
	return nil
}
