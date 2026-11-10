package transcript

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

type ProgressFunc func(message string, percent int)

type Transcriber interface {
	Transcribe(context.Context, domain.Episode, ProgressFunc) (domain.Transcript, error)
}

// PreparedAudioTranscriber consumes the per-episode WAV prepared for analysis
// instead of invoking a second FFmpeg extraction in the ASR backend.
type PreparedAudioTranscriber interface {
	TranscribeWithAudio(context.Context, domain.Episode, audio.PreparedAudio, ProgressFunc) (domain.Transcript, error)
}

type SidecarLoader func(domain.Episode) (domain.Transcript, bool)

type Service struct {
	Backend     Transcriber
	Cache       *PersistentCache
	Sidecar     SidecarLoader
	Identity    func() string
	Diagnostics func(domain.Diagnostic)
}

func (s *Service) Transcribe(ctx context.Context, episode domain.Episode, progress ProgressFunc) (domain.Transcript, error) {
	return s.transcribe(ctx, episode, nil, progress)
}

// TranscribeWithAudio keeps subtitle/cache priority while supplying a cache
// miss to a prepared-audio-aware Whisper backend. Other legacy Transcriber
// implementations retain their existing behavior.
func (s *Service) TranscribeWithAudio(ctx context.Context, episode domain.Episode, prepared audio.PreparedAudio, progress ProgressFunc) (domain.Transcript, error) {
	return s.transcribe(ctx, episode, &prepared, progress)
}

func (s *Service) transcribe(ctx context.Context, episode domain.Episode, prepared *audio.PreparedAudio, progress ProgressFunc) (domain.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	if s.Sidecar != nil {
		if sidecar, ok := s.Sidecar(episode); ok {
			sidecar = clipTranscriptToPrepared(sidecar, prepared)
			if usable(sidecar) {
				return sidecar, nil
			}
		}
		s.diagnose(domain.Diagnostic{Code: "sidecar_unavailable", Message: "有效同名字幕不可用，继续尝试缓存或本地转写", EpisodeNumber: episode.EpisodeNumber})
	}
	identity := ""
	if s.Identity != nil {
		identity = s.Identity()
	} else if identified, ok := s.Backend.(interface{ Identity() string }); ok {
		identity = identified.Identity()
	}
	// A matching successful cache is useful even when the local Whisper runtime
	// is currently unavailable. Do not require a backend until after cache lookup.
	if s.Cache != nil && identity != "" {
		if cached, ok := s.Cache.Load(episode.VideoPath, identity); ok {
			cached = clipTranscriptToPrepared(cached, prepared)
			if usable(cached) {
				return cached, nil
			}
		}
	}
	if s.Backend == nil {
		return domain.Transcript{}, &InvalidResultError{}
	}
	if prepared != nil && (strings.TrimSpace(prepared.SourceVideoPath) == "" ||
		!sameSourcePath(prepared.SourceVideoPath, episode.VideoPath)) {
		return domain.Transcript{}, fmt.Errorf("预备分析音轨缺少源视频身份或与源视频不匹配")
	}
	var result domain.Transcript
	var err error
	if prepared != nil {
		if backend, ok := s.Backend.(PreparedAudioTranscriber); ok {
			result, err = backend.TranscribeWithAudio(ctx, episode, *prepared, progress)
		} else {
			result, err = s.Backend.Transcribe(ctx, episode, progress)
		}
	} else {
		result, err = s.Backend.Transcribe(ctx, episode, progress)
	}
	if err != nil {
		return domain.Transcript{}, err
	}
	result = clipTranscriptToPrepared(result, prepared)
	if !usable(result) || identity == "" || result.Source != identity {
		return domain.Transcript{}, &InvalidResultError{}
	}
	if s.Cache != nil {
		if err := s.Cache.Store(episode.VideoPath, identity, result); err != nil {
			return domain.Transcript{}, err
		}
	}
	return result, nil
}

func clipTranscriptToPrepared(result domain.Transcript, prepared *audio.PreparedAudio) domain.Transcript {
	if prepared == nil {
		return result
	}
	duration := prepared.DurationSec
	if prepared.MediaDurationSec > 0 && prepared.MediaDurationSec < duration {
		duration = prepared.MediaDurationSec
	}
	result.Items = clipWhisperItemsToDuration(result.Items, duration)
	return result
}

func (s *Service) diagnose(diagnostic domain.Diagnostic) {
	if s.Diagnostics != nil {
		s.Diagnostics(diagnostic)
	}
}

func sameSourcePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr == nil && rightErr == nil {
		left, right = leftAbsolute, rightAbsolute
	}
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func usable(result domain.Transcript) bool {
	return strings.TrimSpace(result.Source) != "" && (len(result.Items) > 0 || result.ConfirmedNoSpeech) && result.Validate() == nil
}

// ConfirmNoSpeech applies the visual-only evidence gate. Empty ASR output,
// unknown sources and unreliable VAD statistics are never promoted to success.
func ConfirmNoSpeech(source string, evidence *domain.NoSpeechEvidence) (domain.Transcript, bool) {
	if strings.TrimSpace(source) == "" || evidence == nil || !evidence.Reliable || strings.TrimSpace(evidence.Backend) == "" {
		return domain.Transcript{}, false
	}
	result := domain.Transcript{Source: source, ConfirmedNoSpeech: true, NoSpeechEvidence: evidence}
	return result, result.Validate() == nil
}

type InvalidResultError struct{}

func (*InvalidResultError) Error() string { return "转写结果无效或来源不明确" }
