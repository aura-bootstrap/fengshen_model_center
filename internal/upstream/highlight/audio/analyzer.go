package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	OverlapUnavailableDiagnostic     = "HIGHLIGHT_AUDIO_OVERLAP_UNAVAILABLE"
	SoundEventsUnavailableDiagnostic = "HIGHLIGHT_AUDIO_SOUND_EVENTS_UNAVAILABLE"
	SoundEventsFailedDiagnostic      = "HIGHLIGHT_AUDIO_SOUND_EVENTS_FAILED"
	DefaultMaxSoundEvents            = 256
)

// SoundEventDetector is an optional local injection point. Implementations may
// detect laughter, applause, and music_climax; the analyzer validates and bounds
// every result and never synthesizes an event on detector absence or failure.
type SoundEventDetector interface {
	Detect(context.Context, PreparedAudio) ([]domain.SoundEvent, error)
}

// SignalAnalyzer implements deterministic per-episode PCM analysis. Nil
// OverlapDetector selects the conservative local detector; DisableOverlap and a
// nil SoundEventDetector produce explicit diagnostics instead of hard failures.
type SignalAnalyzer struct {
	MaxWAVBytes        int64
	MaxFrames          int
	VADOptions         VADOptions
	PitchOptions       PitchOptions
	OverlapDetector    OverlapDetector
	DisableOverlap     bool
	SoundEventDetector SoundEventDetector
	MaxSoundEvents     int
}

var _ Analyzer = (*SignalAnalyzer)(nil)
var _ ConfigurableAnalyzer = (*SignalAnalyzer)(nil)

func NewSignalAnalyzer() *SignalAnalyzer { return &SignalAnalyzer{} }

// Analyze retains the legacy entry point, including the receiver's historical
// per-field zero defaults and optional overlap detector injection.
func (a *SignalAnalyzer) Analyze(ctx context.Context, prepared PreparedAudio) (AudioAnalysis, error) {
	return a.analyzePrepared(ctx, prepared, nil)
}

// AnalyzeWithOptions reads the exact prepared WAV and applies a complete public
// quality configuration without exposing PCM, framing, pitch-window, or resource
// controls. Invalid options are rejected before the WAV is opened.
func (a *SignalAnalyzer) AnalyzeWithOptions(ctx context.Context, prepared PreparedAudio, options AnalysisOptions) (AudioAnalysis, error) {
	resolved, err := options.normalized()
	if err != nil {
		return AudioAnalysis{}, WrapPreparationError(prepared.EpisodeNumber, ClassifyFailure(err), err)
	}
	return a.analyzePrepared(ctx, prepared, &resolved)
}

// analyzePrepared rejects any content/timeline identity mismatch before
// producing evidence. A nil options pointer selects the legacy Analyze path.
func (a *SignalAnalyzer) analyzePrepared(ctx context.Context, prepared PreparedAudio, options *AnalysisOptions) (AudioAnalysis, error) {
	episode := prepared.EpisodeNumber
	if err := prepared.Validate(); err != nil {
		return AudioAnalysis{}, WrapPreparationError(episode, ClassifyFailure(err), err)
	}
	if err := ctx.Err(); err != nil {
		return AudioAnalysis{}, err
	}
	maxBytes := int64(0)
	if a != nil {
		maxBytes = a.MaxWAVBytes
	}
	pcm, err := ReadWAV(ctx, prepared.WAVPath, ReadOptions{MaxBytes: maxBytes, ExpectedFormat: prepared.Format})
	if err != nil {
		return AudioAnalysis{}, WrapPreparationError(episode, ClassifyFailure(err), err)
	}
	if int64(len(pcm.Samples)) != prepared.SampleCount || math.Abs(pcm.DurationSec-prepared.DurationSec) > 1/float64(prepared.Format.SampleRate) ||
		pcm.ContentSHA256 != prepared.ContentSHA256 {
		return AudioAnalysis{}, WrapPreparationError(episode, FailureTimeline, ErrTimeline)
	}
	result, err := a.analyzePCMWithOptions(ctx, episode, pcm, &prepared, options)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return AudioAnalysis{}, err
		}
		return AudioAnalysis{}, WrapPreparationError(episode, ClassifyFailure(err), err)
	}
	return result, nil
}

// analyzePCM is deterministic and file-independent for signal unit tests. It
// preserves the legacy analyzer behavior; new callers use AnalyzeWithOptions.
func (a *SignalAnalyzer) analyzePCM(ctx context.Context, episode int, pcm PCM, prepared *PreparedAudio) (AudioAnalysis, error) {
	return a.analyzePCMWithOptions(ctx, episode, pcm, prepared, nil)
}

// analyzePCMWithOptions shares all detection and metric-projection logic between
// legacy and configurable analysis. Sound events require a validated
// PreparedAudio identity and are unavailable when prepared is nil.
func (a *SignalAnalyzer) analyzePCMWithOptions(ctx context.Context, episode int, pcm PCM, prepared *PreparedAudio, options *AnalysisOptions) (AudioAnalysis, error) {
	if episode < 1 {
		return AudioAnalysis{}, fmt.Errorf("%w: episode number must be positive", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return AudioAnalysis{}, err
	}
	maxFrames := 0
	if a != nil {
		maxFrames = a.MaxFrames
	}
	frames, err := FramePCM(ctx, pcm, FramingOptions{MaxFrames: maxFrames})
	if err != nil {
		return AudioAnalysis{}, err
	}
	vadOption := VADOptions{}
	pitchOption := PitchOptions{}
	if options != nil {
		vadOption = options.VAD
		pitchOption = options.Pitch
		// The configurable path validates without legacy per-field defaulting so
		// explicit zero VAD thresholds survive. Pitch work remains bounded by the
		// analyzer's existing frame limit, never by public quality configuration.
		vadOption, err = vadOption.validated()
		if err != nil {
			return AudioAnalysis{}, err
		}
		pitchOption.MaxFrames = maxFrames
		if pitchOption.MaxFrames == 0 {
			pitchOption.MaxFrames = DefaultMaxFrames
		}
		pitchOption, err = pitchOption.validated()
		if err != nil {
			return AudioAnalysis{}, err
		}
	} else {
		if a != nil {
			vadOption = a.VADOptions
			pitchOption = a.PitchOptions
		}
		vadOption, err = vadOption.normalized()
		if err != nil {
			return AudioAnalysis{}, err
		}
	}
	var vad VADResult
	if options != nil {
		vad, err = detectVADValidated(ctx, frames, vadOption)
	} else {
		vad, err = DetectVAD(ctx, frames, vadOption)
	}
	if err != nil {
		return AudioAnalysis{}, err
	}
	pauses, err := DerivePauses(vad.Speech, vadOption.PauseThresholdSec)
	if err != nil {
		return AudioAnalysis{}, err
	}
	pitchedFrames, err := EstimatePitch(ctx, pcm, vad.Frames, pitchOption)
	if err != nil {
		return AudioAnalysis{}, err
	}
	for index := range pitchedFrames {
		if pitchedFrames[index].Speech {
			continue
		}
		pitchedFrames[index].PitchValid = false
		pitchedFrames[index].PitchHz = 0
		pitchedFrames[index].PitchConfidence = 0
		pitchedFrames[index].pitch.hasMultiPitch = false
	}

	analysis := AudioAnalysis{
		EpisodeNumber: episode,
		DurationSec:   pcm.DurationSec,
		VAD:           domain.VADStatus{Backend: EnergyVADBackend, Reliable: vad.Reliable},
		Coverage:      vad.Coverage,
		Speech:        vad.Speech, Silence: vad.Silence, Pauses: pauses,
		Overlaps: make([]domain.OverlapInterval, 0), SoundEvents: make([]domain.SoundEvent, 0),
		Frames: pitchedFrames,
	}
	if analysis.DurationSec == 0 {
		analysis.DurationSec = float64(len(pcm.Samples)) / float64(pcm.Format.SampleRate)
	}
	if err := a.addOverlapEvidence(ctx, &analysis, options); err != nil {
		return AudioAnalysis{}, err
	}
	analysis.NoSpeechEvidence = NoSpeechEvidenceFor(analysis)
	if err := a.addSoundEvents(ctx, &analysis, prepared); err != nil {
		return AudioAnalysis{}, err
	}
	return analysis, nil
}

func (a *SignalAnalyzer) addOverlapEvidence(ctx context.Context, analysis *AudioAnalysis, options *AnalysisOptions) error {
	if a != nil && a.DisableOverlap {
		analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(OverlapUnavailableDiagnostic, "重叠说话检测已禁用，主音频分析继续", analysis.EpisodeNumber))
		return nil
	}
	var detector OverlapDetector = ConservativeOverlapDetector{}
	if options != nil {
		detector = options.Overlap
	}
	// Preserve the existing local injection point. The built-in conservative
	// detector receives AnalysisOptions; custom detectors keep their own config.
	if a != nil && a.OverlapDetector != nil {
		detector = a.OverlapDetector
	}
	overlaps, err := detector.DetectOverlap(ctx, analysis.Frames)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if errors.Is(err, ErrResourceLimit) || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrAnalysis) || errors.Is(err, ErrTimeline) {
			return err
		}
		analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(OverlapUnavailableDiagnostic, "重叠说话检测不可用，主音频分析继续", analysis.EpisodeNumber))
		return nil
	}
	if len(overlaps) > DefaultMaxOverlaps {
		return fmt.Errorf("%w: overlap detector returned too many safety intervals", ErrResourceLimit)
	}
	validated := append([]domain.OverlapInterval(nil), overlaps...)
	sort.SliceStable(validated, func(i, j int) bool {
		if validated[i].StartSec == validated[j].StartSec {
			return validated[i].EndSec < validated[j].EndSec
		}
		return validated[i].StartSec < validated[j].StartSec
	})
	for index, overlap := range validated {
		if err := overlap.Validate(); err != nil || overlap.EndSec > analysis.DurationSec+1e-9 ||
			index > 0 && overlap.StartSec < validated[index-1].EndSec {
			return fmt.Errorf("%w: overlap detector returned invalid or duplicate safety intervals", ErrAnalysis)
		}
	}
	analysis.Overlaps = validated
	return nil
}

func (a *SignalAnalyzer) addSoundEvents(ctx context.Context, analysis *AudioAnalysis, prepared *PreparedAudio) error {
	var detector SoundEventDetector
	if a != nil {
		detector = a.SoundEventDetector
	}
	if detector == nil || prepared == nil {
		analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(SoundEventsUnavailableDiagnostic, "未配置可选声音事件检测器，主音频分析继续", analysis.EpisodeNumber))
		return nil
	}
	maxEvents := DefaultMaxSoundEvents
	if a != nil && a.MaxSoundEvents != 0 {
		maxEvents = a.MaxSoundEvents
	}
	if maxEvents < 1 || maxEvents > DefaultMaxSoundEvents {
		return fmt.Errorf("%w: maximum sound event count must be between 1 and %d", ErrInvalidRequest, DefaultMaxSoundEvents)
	}
	events, err := detector.Detect(ctx, *prepared)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(SoundEventsFailedDiagnostic, "可选声音事件检测失败，主音频分析继续", analysis.EpisodeNumber))
		return nil
	}
	if len(events) > maxEvents {
		analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(SoundEventsFailedDiagnostic, "可选声音事件超出资源上限，已丢弃事件并继续主音频分析", analysis.EpisodeNumber))
		return nil
	}
	validated := append([]domain.SoundEvent(nil), events...)
	sort.SliceStable(validated, func(i, j int) bool {
		if validated[i].StartSec == validated[j].StartSec {
			if validated[i].EndSec == validated[j].EndSec {
				return validated[i].Type < validated[j].Type
			}
			return validated[i].EndSec < validated[j].EndSec
		}
		return validated[i].StartSec < validated[j].StartSec
	})
	for index, event := range validated {
		duplicate := index > 0 && event.Type == validated[index-1].Type &&
			event.StartSec == validated[index-1].StartSec && event.EndSec == validated[index-1].EndSec
		if err := event.Validate(); err != nil || event.EndSec > analysis.DurationSec+1e-9 || duplicate {
			analysis.Diagnostics = append(analysis.Diagnostics, degradationDiagnostic(SoundEventsFailedDiagnostic, "可选声音事件结果无效，已丢弃事件并继续主音频分析", analysis.EpisodeNumber))
			return nil
		}
	}
	analysis.SoundEvents = validated
	return nil
}

func degradationDiagnostic(code, message string, episode int) domain.Diagnostic {
	return domain.Diagnostic{Code: code, Message: message, EpisodeNumber: episode}
}
