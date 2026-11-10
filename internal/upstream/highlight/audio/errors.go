package audio

import (
	"context"
	"errors"
	"fmt"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const DiagnosticCode = "HIGHLIGHT_AUDIO_ANALYSIS_FAILED"

var (
	ErrInvalidRequest = errors.New("invalid audio preparation request")
	ErrUnsafeOutput   = errors.New("unsafe audio output path")
	ErrNoAudioStream  = errors.New("source has no audio stream")
	ErrExtraction     = errors.New("audio extraction failed")
	ErrWAVRead        = errors.New("audio WAV read failed")
	ErrWAVFormat      = errors.New("unexpected or corrupt WAV format")
	ErrWAVTooLarge    = errors.New("audio WAV exceeds the configured limit")
	ErrTimeline       = errors.New("audio timeline is inconsistent")
	ErrEmptyAudio     = errors.New("analysis audio is empty")
	ErrAnalysis       = errors.New("audio analysis failed")
	ErrResourceLimit  = errors.New("audio analysis resource limit exceeded")
)

// FailureKind is a stable, non-sensitive subcategory for diagnostics.
type FailureKind string

const (
	FailureRequest    FailureKind = "request"
	FailurePath       FailureKind = "path"
	FailureProbe      FailureKind = "probe"
	FailureExtraction FailureKind = "extraction"
	FailureFormat     FailureKind = "format"
	FailureTimeline   FailureKind = "timeline"
	FailureAnalysis   FailureKind = "analysis"
)

// PreparationError classifies a failure without exposing command stderr,
// source paths, transcript contents, or local media paths in its Error text.
type PreparationError struct {
	EpisodeNumber int
	Kind          FailureKind
	Err           error
}

func (e *PreparationError) Error() string {
	if e == nil {
		return DiagnosticCode
	}
	if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
		return DiagnosticCode + ": 音频分析已取消"
	}
	message := "音频分析失败"
	switch e.Kind {
	case FailureRequest:
		message = "音频准备请求无效"
	case FailurePath:
		message = "音频临时路径不安全"
	case FailureProbe:
		message = "媒体时长探测失败"
	case FailureExtraction:
		message = "音轨提取失败"
	case FailureFormat:
		message = "音轨 PCM/WAV 格式无效"
	case FailureTimeline:
		message = "音轨媒体时间轴不一致"
	case FailureAnalysis:
		message = "音频分析失败"
	}
	return fmt.Sprintf("%s: 第 %d 集%s", DiagnosticCode, e.EpisodeNumber, message)
}

func (e *PreparationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Code returns the stable cross-layer audio diagnostic code.
func (e *PreparationError) Code() string { return DiagnosticCode }

// Diagnostic returns a persistable, sanitized episode diagnostic.
func (e *PreparationError) Diagnostic() domain.Diagnostic {
	if e == nil {
		return domain.Diagnostic{Code: DiagnosticCode, Message: "音频分析失败"}
	}
	return domain.Diagnostic{
		Code:          DiagnosticCode,
		Message:       e.Error(),
		EpisodeNumber: e.EpisodeNumber,
	}
}

// WrapPreparationError adds the stable audio classification while retaining
// errors.Is/errors.As access to cancellation and low-level sentinel errors.
func WrapPreparationError(episodeNumber int, kind FailureKind, err error) error {
	if err == nil {
		return nil
	}
	var existing *PreparationError
	if errors.As(err, &existing) {
		return err
	}
	return &PreparationError{EpisodeNumber: episodeNumber, Kind: kind, Err: err}
}

// ClassifyFailure maps implementation failures to stable preparation classes.
func ClassifyFailure(err error) FailureKind {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidRequest):
		return FailureRequest
	case errors.Is(err, ErrUnsafeOutput):
		return FailurePath
	case errors.Is(err, ErrWAVRead), errors.Is(err, ErrWAVFormat), errors.Is(err, ErrWAVTooLarge):
		return FailureFormat
	case errors.Is(err, ErrTimeline), errors.Is(err, ErrEmptyAudio):
		return FailureTimeline
	case errors.Is(err, ErrNoAudioStream), errors.Is(err, ErrExtraction):
		return FailureExtraction
	default:
		return FailureAnalysis
	}
}

// DiagnosticForError classifies arbitrary preparation failures at the episode
// boundary. A failure never produces NoSpeechEvidence or ConfirmedNoSpeech.
func DiagnosticForError(episodeNumber int, err error) domain.Diagnostic {
	if err == nil {
		return domain.Diagnostic{}
	}
	var preparationError *PreparationError
	if errors.As(err, &preparationError) {
		return preparationError.Diagnostic()
	}
	wrapped := &PreparationError{EpisodeNumber: episodeNumber, Kind: ClassifyFailure(err), Err: err}
	return wrapped.Diagnostic()
}
