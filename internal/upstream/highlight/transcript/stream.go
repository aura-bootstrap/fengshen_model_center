package transcript

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	DefaultStreamWindowSec             = 30.0
	DefaultStreamStepSec               = 2.0
	DefaultStreamStableRounds          = 2
	DefaultStreamTimestampToleranceSec = 0.20
	DefaultStreamFinalizeDelaySec      = 2.0
)

var (
	ErrStreamingBackendUnavailable = errors.New("原生流式 Whisper 后端不可用")
	ErrStreamProtocol              = errors.New("原生流式 Whisper 协议错误")
)

// StreamOptions controls stable-prefix publication. WindowSec and StepSec are
// consumed by the long-lived whisper.cpp decoder; they never cause independent
// whisper-cli invocations. Zero values select defaults only for fields where
// zero is not itself a valid explicit setting.
type StreamOptions struct {
	WindowSec             float64 `json:"window_sec"`
	StepSec               float64 `json:"step_sec"`
	StableRounds          int     `json:"stable_rounds"`
	TimestampToleranceSec float64 `json:"timestamp_tolerance_sec"`
	FinalizeDelaySec      float64 `json:"finalize_delay_sec"`
}

func DefaultStreamOptions() StreamOptions {
	return StreamOptions{
		WindowSec:             DefaultStreamWindowSec,
		StepSec:               DefaultStreamStepSec,
		StableRounds:          DefaultStreamStableRounds,
		TimestampToleranceSec: DefaultStreamTimestampToleranceSec,
		FinalizeDelaySec:      DefaultStreamFinalizeDelaySec,
	}
}

func (o StreamOptions) WithDefaults() StreamOptions {
	if o.WindowSec == 0 {
		o.WindowSec = DefaultStreamWindowSec
	}
	if o.StepSec == 0 {
		o.StepSec = DefaultStreamStepSec
	}
	if o.StableRounds == 0 {
		o.StableRounds = DefaultStreamStableRounds
	}
	return o
}

func (o StreamOptions) Validate() error {
	o = o.WithDefaults()
	if !domain.Finite(o.WindowSec) || o.WindowSec < 5 || o.WindowSec > 120 {
		return fmt.Errorf("stream_window_sec 须在 5..120 之间")
	}
	if !domain.Finite(o.StepSec) || o.StepSec < 0.5 || o.StepSec > 10 || o.StepSec > o.WindowSec {
		return fmt.Errorf("stream_step_sec 须在 0.5..10 之间且不大于 stream_window_sec")
	}
	if o.StableRounds < 1 || o.StableRounds > 5 {
		return fmt.Errorf("stream_stable_rounds 须为 1..5")
	}
	if !domain.Finite(o.TimestampToleranceSec) || o.TimestampToleranceSec < 0 || o.TimestampToleranceSec > 2 {
		return fmt.Errorf("stream_timestamp_tolerance_sec 须在 0..2 之间")
	}
	if !domain.Finite(o.FinalizeDelaySec) || o.FinalizeDelaySec < 0 || o.FinalizeDelaySec > 10 {
		return fmt.Errorf("stream_finalize_delay_sec 须在 0..10 之间")
	}
	return nil
}

// StreamCommit is the only transcription output visible to the streaming
// pipeline. Items contains newly committed, immutable statements, never a
// hypothesis. Empty Items with a later CommittedThroughSec represents verified
// non-speech progress and can make pending candidates ready.
type StreamCommit struct {
	EpisodeNumber       int                     `json:"episode_number"`
	Revision            uint64                  `json:"revision"`
	CommittedThroughSec float64                 `json:"committed_through_sec"`
	Items               []domain.TranscriptItem `json:"items"`
	Final               bool                    `json:"final"`
}

// StreamHypothesis is private backend evidence presented to the stable
// committer. Callers outside the ASR adapter should consume StreamCommit only.
type StreamHypothesis struct {
	EpisodeNumber       int
	Revision            uint64
	CommittedThroughSec float64
	Items               []domain.TranscriptItem
	Final               bool
}

// StreamingTranscriber keeps one native decoding context alive for the whole
// episode. Implementations must not emulate this interface by splitting an
// episode into WAV files and repeatedly invoking batch whisper-cli.
type StreamingTranscriber interface {
	TranscribeStream(
		context.Context,
		domain.Episode,
		audio.PreparedAudio,
		StreamOptions,
		func(StreamCommit) error,
	) error
}

// StreamProtocolError exposes a stable, sanitized classification. Backend
// stdout/stderr, transcript text, and local media contents are never included.
type StreamProtocolError struct {
	Code string
}

func (e *StreamProtocolError) Error() string {
	code := "protocol"
	if e != nil && strings.TrimSpace(e.Code) != "" {
		code = e.Code
	}
	return fmt.Sprintf("%s: %s", ErrStreamProtocol, code)
}

func (e *StreamProtocolError) Unwrap() error { return ErrStreamProtocol }

func streamProtocolError(code string) error {
	return &StreamProtocolError{Code: code}
}

func cloneStreamItem(item domain.TranscriptItem) domain.TranscriptItem {
	cloned := item
	if item.Probability != nil {
		probability := *item.Probability
		cloned.Probability = &probability
	}
	cloned.Words = make([]domain.TranscriptWord, len(item.Words))
	for index, word := range item.Words {
		cloned.Words[index] = word
		if word.Probability != nil {
			probability := *word.Probability
			cloned.Words[index].Probability = &probability
		}
	}
	return cloned
}

func cloneStreamItems(items []domain.TranscriptItem) []domain.TranscriptItem {
	cloned := make([]domain.TranscriptItem, len(items))
	for index, item := range items {
		cloned[index] = cloneStreamItem(item)
	}
	return cloned
}

func cloneStreamCommit(commit StreamCommit) StreamCommit {
	commit.Items = cloneStreamItems(commit.Items)
	return commit
}
