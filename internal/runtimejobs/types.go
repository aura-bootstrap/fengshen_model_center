// Package runtimejobs owns bounded asynchronous work, not authentication, media
// files or OS processes. Callers pass session/task ownership and verify frozen
// binding identities before Submit. Executor must stop and join all work it owns
// before returning; implementations must never adopt external processes.
package runtimejobs

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

const MaxBodyBytes = 64 << 10
const MaxResultBytes = 4 << 20

type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string              { return e.Code }
func failure(status int, code string) error { return &Error{status, code} }

type State string

const (
	Queued    State = "queued"
	Loading   State = "loading"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled }

type Stage string

const (
	StageLoading Stage = "loading"
	StageRunning Stage = "running"
)

// Progress deliberately has no free-form log or text field.
type Progress struct {
	Stage     Stage         `json:"stage"`
	Completed int           `json:"completed"`
	Total     int           `json:"total"`
	Commit    *StreamCommit `json:"commit,omitempty"`
}
type Emit func(Progress) error

// Executor is supplied by the center, never selected by an HTTP request. It
// resolves opaque assets, rechecks bindings and validates capability-specific
// semantics. Close releases only its own resources; it is called after all
// Execute calls have returned. Execute must honor ctx and not retain emit.
type Executor interface {
	Execute(context.Context, Request, Emit) (Result, error)
	Close() error
}
type Request struct {
	JobID, SessionID, TaskID, RequestID string
	Binding                             integration.ExecutionBindingRef
	Body                                Body
	BodySHA256                          string
}
type TranscribeInput struct {
	AssetID       string                            `json:"asset_id"`
	Stream        bool                              `json:"stream"`
	VAD           bool                              `json:"vad"`
	OffsetMS      int64                             `json:"offset_ms"`
	EpisodeNumber int                               `json:"episode_number"`
	Options       *integration.RuntimeStreamOptions `json:"options,omitempty"`
}
type VisionInput struct {
	AssetID   string     `json:"asset_id"`
	Candidate ScoreInput `json:"candidate"`
}
type SeparateInput struct {
	AssetID string `json:"asset_id"`
}
type ConvertInput struct {
	AssetID          string `json:"asset_id"`
	ReferenceAssetID string `json:"reference_asset_id"`
	Pitch            int    `json:"pitch"`
}
type FaceInput struct {
	AssetID string `json:"asset_id"`
}
type Body struct {
	Component  integration.ExecutionComponentRef `json:"component"`
	Transcribe *TranscribeInput                  `json:"transcribe,omitempty"`
	Score      *ScoreInput                       `json:"score,omitempty"`
	Vision     *VisionInput                      `json:"vision,omitempty"`
	Separate   *SeparateInput                    `json:"separate,omitempty"`
	Convert    *ConvertInput                     `json:"convert,omitempty"`
	Face       *FaceInput                        `json:"face,omitempty"`
}

// DecodeBody rejects unknown fields, trailing JSON and oversized bodies. It does
// not perform authorization or resolve any asset to a local path.
func DecodeBody(data []byte) (Body, error) {
	var b Body
	if len(data) > MaxBodyBytes {
		return b, failure(413, "center_request_too_large")
	}
	if decodeStrict(data, &b) != nil {
		return b, failure(422, "center_request_invalid")
	}
	return b, nil
}
func opaque(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (b Body) validate(scene string) bool {
	if !validUTF8Value(reflect.ValueOf(b)) || b.Component.Validate(scene) != nil {
		return false
	}
	n := 0
	for _, ok := range []bool{b.Transcribe != nil, b.Score != nil, b.Vision != nil, b.Separate != nil, b.Convert != nil, b.Face != nil} {
		if ok {
			n++
		}
	}
	if n != 1 {
		return false
	}
	switch scene {
	case "highlight.transcribe":
		return b.Transcribe != nil && (b.Transcribe.Options == nil || b.Transcribe.Stream && b.Transcribe.Options.Validate() == nil) && b.Transcribe.EpisodeNumber > 0 && opaque(b.Transcribe.AssetID) && b.Transcribe.OffsetMS >= 0 && b.Transcribe.OffsetMS <= 86400000 && ((b.Transcribe.Stream && b.Component.ID == "whisper-stream") || (!b.Transcribe.Stream && b.Component.ID == "whisper-batch"))
	case "highlight.score":
		return b.Score != nil && b.Score.valid()
	case "highlight.vision":
		return b.Vision != nil && opaque(b.Vision.AssetID) && b.Vision.Candidate.valid()
	case "timbre.separate":
		return b.Separate != nil && opaque(b.Separate.AssetID)
	case "timbre.convert":
		return b.Convert != nil && opaque(b.Convert.AssetID) && opaque(b.Convert.ReferenceAssetID) && b.Convert.Pitch >= -24 && b.Convert.Pitch <= 24
	case "image.face":
		return b.Face != nil && opaque(b.Face.AssetID)
	}
	return false
}

// MediaResult references already validated, task-owned output, never a path.
// The adapter/spool owner must verify bytes, SHA256 and actual WAV properties.
type MediaResult struct {
	ID         string `json:"id"`
	Bytes      string `json:"bytes"`
	SHA256     string `json:"sha256"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	DurationMS int64  `json:"duration_ms"`
}
type FaceResult string

const (
	Frontal FaceResult = "frontal"
	Profile FaceResult = "profile"
	NoFace  FaceResult = "no_face"
)

type Result struct {
	Component integration.ExecutionComponentRef `json:"component"`
	Items     []TranscriptItem                  `json:"items,omitempty"`
	Stream    *StreamCommit                     `json:"stream,omitempty"`
	Events    []*RawEvent                       `json:"events"`
	Media     []MediaResult                     `json:"media,omitempty"`
	Face      FaceResult                        `json:"face,omitempty"`
}

func unit(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func (r Result) validate(req Request) bool {
	if !validUTF8Value(reflect.ValueOf(r)) || r.Component != req.Body.Component {
		return false
	}
	n := 0
	for _, ok := range []bool{r.Items != nil, r.Stream != nil, r.Events != nil, r.Media != nil, r.Face != ""} {
		if ok {
			n++
		}
	}
	if n != 1 {
		return false
	}
	switch req.Binding.Scene {
	case "highlight.transcribe":
		if req.Body.Transcribe.Stream {
			return r.Stream != nil && r.Stream.Final && commitValid(*r.Stream, req.Body.Transcribe, nil)
		}
		return r.Items != nil && len(r.Items) > 0 && itemsValid(r.Items, float64(req.Body.Transcribe.OffsetMS)/1000, 86400)
	case "highlight.score", "highlight.vision":
		var input ScoreInput
		if req.Body.Score != nil {
			input = *req.Body.Score
		} else {
			input = req.Body.Vision.Candidate
		}
		if r.Events == nil || len(r.Events) > 128 {
			return false
		}
		for _, e := range r.Events {
			if !eventValid(e, input) {
				return false
			}
		}
		return true
	case "timbre.separate", "timbre.convert":
		want := 1
		if req.Binding.Scene == "timbre.separate" {
			want = 2
		}
		if len(r.Media) != want {
			return false
		}
		seen := map[string]bool{}
		for _, v := range r.Media {
			size, err := strconv.ParseInt(v.Bytes, 10, 64)
			if err != nil || strconv.FormatInt(size, 10) != v.Bytes || !opaque(v.ID) || seen[v.ID] || size <= 0 || size > 1<<30 || !digest(v.SHA256) || v.SampleRate < 8000 || v.SampleRate > 192000 || v.Channels < 1 || v.Channels > 2 || v.DurationMS <= 0 || v.DurationMS > 86400000 {
				return false
			}
			seen[v.ID] = true
		}
		return true
	case "image.face":
		return r.Face == Frontal || r.Face == Profile || r.Face == NoFace
	}
	return false
}

type Timer interface{ Stop() bool }

// Clock methods must be concurrency-safe. AfterFunc must not invoke fn inline.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) Timer
}
type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

type Options struct {
	Clock                                            Clock
	QueueTimeout, RunTimeout, HaarTimeout, Retention time.Duration
	MaxJobs                                          int
	MaxRetainedBytes                                 int64
	// ScoreSlots bounds concurrent highlight.score/vision jobs sharing one
	// persistent llama-server (--parallel N). Transcribe/timbre stay exclusive.
	ScoreSlots int
}

func (o *Options) defaults() error {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.QueueTimeout == 0 {
		o.QueueTimeout = 2 * time.Minute
	}
	if o.RunTimeout == 0 {
		o.RunTimeout = 30 * time.Minute
	}
	if o.HaarTimeout == 0 {
		o.HaarTimeout = 10 * time.Second
	}
	if o.Retention == 0 {
		o.Retention = 30 * time.Minute
	}
	if o.MaxJobs == 0 {
		o.MaxJobs = 128
	}
	if o.MaxRetainedBytes == 0 {
		o.MaxRetainedBytes = 128 * MaxResultBytes
	}
	if o.ScoreSlots == 0 {
		o.ScoreSlots = 4
	}
	if o.QueueTimeout < 0 || o.QueueTimeout > 2*time.Minute || o.RunTimeout < 0 || o.RunTimeout > 30*time.Minute || o.HaarTimeout < 0 || o.HaarTimeout > 10*time.Second || o.Retention < 0 || o.Retention > 30*time.Minute || o.MaxJobs < 1 || o.MaxJobs > 128 || o.MaxRetainedBytes < MaxResultBytes || o.ScoreSlots < 1 || o.ScoreSlots > 4 {
		return errors.New("center_options_invalid")
	}
	return nil
}

type Snapshot struct {
	ID, SessionID, TaskID, RequestID string
	Binding                          integration.ExecutionBindingRef
	BodySHA256                       string
	State                            State
	Code                             string
	CreatedAt, StartedAt, FinishedAt time.Time
	Cursor                           uint64
	Acknowledged                     bool
}
type Event struct {
	Sequence uint64
	At       time.Time
	State    State
	Code     string
	Progress *Progress
	Commit   *StreamCommit
}
