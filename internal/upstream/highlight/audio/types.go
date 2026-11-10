// Package audio prepares and validates the local, per-episode analysis track.
// Prepared media is deliberately process-local and must never become scoring
// or catalog wire data.
package audio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	AnalysisSampleRate    = 16000
	AnalysisChannels      = 1
	AnalysisBitsPerSample = 16
)

// PCMFormat identifies the fixed format consumed by Whisper and analysis.
type PCMFormat struct {
	SampleRate    int `json:"-"`
	Channels      int `json:"-"`
	BitsPerSample int `json:"-"`
}

// AnalysisFormat returns the canonical 16 kHz, mono, signed 16-bit PCM format.
func AnalysisFormat() PCMFormat {
	return PCMFormat{
		SampleRate:    AnalysisSampleRate,
		Channels:      AnalysisChannels,
		BitsPerSample: AnalysisBitsPerSample,
	}
}

func (f PCMFormat) normalized() (PCMFormat, error) {
	if f == (PCMFormat{}) {
		return AnalysisFormat(), nil
	}
	if f != AnalysisFormat() {
		return PCMFormat{}, fmt.Errorf("%w: analysis audio must be 16 kHz mono 16-bit PCM", ErrInvalidRequest)
	}
	return f, nil
}

// Validate rejects formats that the bounded PCM reader and Whisper do not use.
func (f PCMFormat) Validate() error {
	if f != AnalysisFormat() {
		return fmt.Errorf("%w: unexpected PCM format", ErrWAVFormat)
	}
	return nil
}

// AudioPrepareRequest describes one episode's process-local analysis track.
// Zero Format selects AnalysisFormat; any other non-zero format is rejected.
type AudioPrepareRequest struct {
	SourceVideoPath  string    `json:"-"`
	EpisodeNumber    int       `json:"-"`
	TaskTempDir      string    `json:"-"`
	MediaDurationSec float64   `json:"-"`
	Format           PCMFormat `json:"-"`
}

// MarshalJSON prevents local source/temp paths from becoming protocol data.
func (AudioPrepareRequest) MarshalJSON() ([]byte, error) {
	return nil, ErrLocalMediaSerialization
}

// Validate checks request identity, timeline bounds, and the fixed PCM format.
func (r AudioPrepareRequest) Validate() error {
	_, err := r.normalized()
	return err
}

func (r AudioPrepareRequest) normalized() (AudioPrepareRequest, error) {
	if strings.TrimSpace(r.SourceVideoPath) == "" {
		return AudioPrepareRequest{}, fmt.Errorf("%w: source video path is required", ErrInvalidRequest)
	}
	if r.EpisodeNumber < 1 {
		return AudioPrepareRequest{}, fmt.Errorf("%w: episode number must be positive", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.TaskTempDir) == "" {
		return AudioPrepareRequest{}, fmt.Errorf("%w: task temporary directory is required", ErrInvalidRequest)
	}
	if math.IsNaN(r.MediaDurationSec) || math.IsInf(r.MediaDurationSec, 0) || r.MediaDurationSec < 0 {
		return AudioPrepareRequest{}, fmt.Errorf("%w: media duration must be finite and non-negative", ErrInvalidRequest)
	}
	format, err := r.Format.normalized()
	if err != nil {
		return AudioPrepareRequest{}, err
	}
	r.Format = format
	return r, nil
}

// PreparedAudio contains local media identity only. It must be passed directly
// to local consumers, never embedded in a request, snapshot, or catalog.
type PreparedAudio struct {
	SourceVideoPath  string    `json:"-"`
	WAVPath          string    `json:"-"`
	EpisodeNumber    int       `json:"-"`
	DurationSec      float64   `json:"-"`
	MediaDurationSec float64   `json:"-"`
	SampleCount      int64     `json:"-"`
	Format           PCMFormat `json:"-"`
	ContentSHA256    string    `json:"-"`
}

var ErrLocalMediaSerialization = errors.New("local prepared audio cannot be serialized")

// MarshalJSON fails closed if local media is accidentally added to a wire or
// persistent structure. Use an explicit, privacy-reviewed projection instead.
func (PreparedAudio) MarshalJSON() ([]byte, error) {
	return nil, ErrLocalMediaSerialization
}

// Validate verifies the complete prepared identity returned by AudioPreparer.
func (p PreparedAudio) Validate() error {
	if p.EpisodeNumber < 1 || strings.TrimSpace(p.WAVPath) == "" {
		return fmt.Errorf("%w: prepared episode and WAV path are required", ErrInvalidRequest)
	}
	if !positiveFinite(p.DurationSec) || p.SampleCount < 1 {
		return fmt.Errorf("%w: prepared audio is empty or has an invalid duration", ErrEmptyAudio)
	}
	sampleDuration := float64(p.SampleCount) / float64(p.Format.SampleRate)
	if p.Format.SampleRate != 0 && math.Abs(sampleDuration-p.DurationSec) > 1/float64(p.Format.SampleRate) {
		return fmt.Errorf("%w: WAV duration does not match its PCM sample count", ErrTimeline)
	}
	if p.MediaDurationSec < 0 || math.IsNaN(p.MediaDurationSec) || math.IsInf(p.MediaDurationSec, 0) {
		return fmt.Errorf("%w: invalid prepared media duration", ErrTimeline)
	}
	if err := p.Format.Validate(); err != nil {
		return err
	}
	if len(p.ContentSHA256) != 64 || strings.ToLower(p.ContentSHA256) != p.ContentSHA256 {
		return fmt.Errorf("%w: prepared content identity is required", ErrInvalidRequest)
	}
	if _, err := hex.DecodeString(p.ContentSHA256); err != nil {
		return fmt.Errorf("%w: prepared content identity is not SHA-256", ErrInvalidRequest)
	}
	return nil
}

// AudioPreparer extracts and validates at most one shared WAV per episode.
type AudioPreparer interface {
	Prepare(context.Context, AudioPrepareRequest) (PreparedAudio, error)
}

// AudioAnalysis contains compact timeline evidence plus process-local frames
// used to project statement/candidate metrics. Frames are never wire data.
type AudioAnalysis struct {
	EpisodeNumber    int                      `json:"episode_number"`
	DurationSec      float64                  `json:"duration_sec"`
	VAD              domain.VADStatus         `json:"vad"`
	Coverage         AnalysisCoverage         `json:"coverage"`
	Speech           []domain.AudioInterval   `json:"speech"`
	Silence          []domain.AudioInterval   `json:"silence"`
	Pauses           []domain.AudioInterval   `json:"pauses"`
	Overlaps         []domain.OverlapInterval `json:"overlaps"`
	SoundEvents      []domain.SoundEvent      `json:"sound_events"`
	NoSpeechEvidence *domain.NoSpeechEvidence `json:"no_speech_evidence,omitempty"`
	Diagnostics      []domain.Diagnostic      `json:"diagnostics,omitempty"`
	Frames           []PCMFrame               `json:"-"`
}

// Analyzer consumes the exact same prepared WAV used by Whisper.
type Analyzer interface {
	Analyze(context.Context, PreparedAudio) (AudioAnalysis, error)
}

// ConfigurableAnalyzer is the opt-in interface used by new versioned analysis
// chains. Existing Analyzer implementations and fakes remain valid unchanged.
type ConfigurableAnalyzer interface {
	AnalyzeWithOptions(context.Context, PreparedAudio, AnalysisOptions) (AudioAnalysis, error)
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// Keep the compiler honest about the intended local-only JSON behavior when
// PreparedAudio is nested behind an interface as well as behind a concrete field.
var _ json.Marshaler = PreparedAudio{}
var _ json.Marshaler = AudioPrepareRequest{}
