package transcript

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

// ReplayStreamingTranscriber republishes an already complete, usable transcript
// through the stable-prefix interface. It never starts Whisper.
type ReplayStreamingTranscriber struct {
	Transcript domain.Transcript
}

func (r ReplayStreamingTranscriber) TranscribeStream(ctx context.Context, episode domain.Episode, prepared audio.PreparedAudio, options StreamOptions, emit func(StreamCommit) error) error {
	options = options.WithDefaults()
	if err := options.Validate(); err != nil {
		return err
	}
	if emit == nil {
		return fmt.Errorf("流式稳定提交回调不能为空")
	}
	if err := validateStreamPreparedIdentity(episode, prepared); err != nil {
		return err
	}
	result, err := validatedReplayTranscript(r.Transcript, prepared)
	if err != nil {
		return err
	}
	items := result.Items

	committedThrough := 0.0
	nextItem := 0
	revision := uint64(0)
	for nextItem < len(items) && items[nextItem].EndSec+options.FinalizeDelaySec <= prepared.DurationSec+1e-9 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := items[nextItem]
		target := math.Min(prepared.DurationSec, item.EndSec+options.FinalizeDelaySec)
		if target < committedThrough {
			target = committedThrough
		}
		newItems := make([]domain.TranscriptItem, 0, 1)
		for nextItem < len(items) && items[nextItem].EndSec+options.FinalizeDelaySec <= target+1e-9 {
			newItems = append(newItems, cloneStreamItem(items[nextItem]))
			nextItem++
		}
		revision++
		commit := StreamCommit{
			EpisodeNumber: episode.EpisodeNumber, Revision: revision,
			CommittedThroughSec: target, Items: newItems,
		}
		if err := emit(cloneStreamCommit(commit)); err != nil {
			return err
		}
		committedThrough = target
	}
	final := StreamCommit{
		EpisodeNumber: episode.EpisodeNumber, Revision: revision + 1,
		CommittedThroughSec: prepared.DurationSec, Items: cloneStreamItems(items[nextItem:]), Final: true,
	}
	return emit(final)
}

func validatedReplayTranscript(input domain.Transcript, prepared audio.PreparedAudio) (domain.Transcript, error) {
	result := clipTranscriptToPrepared(input, &prepared)
	if !usable(result) {
		return domain.Transcript{}, &InvalidResultError{}
	}
	items, _, err := normalizeStreamItems(result.Items, prepared.DurationSec, false)
	if err != nil {
		return domain.Transcript{}, &InvalidResultError{}
	}
	result.Items = items
	if err := audio.ValidateTimeline(prepared, prepared.MediaDurationSec, result); err != nil {
		return domain.Transcript{}, fmt.Errorf("完整重放转写时间轴无效: %w", err)
	}
	return result, nil
}

// StreamingService preserves the existing sidecar/cache priority. A real ASR
// miss requires a native StreamingTranscriber; a legacy batch Transcriber is
// deliberately not accepted as a fallback.
type StreamingService struct {
	Backend     StreamingTranscriber
	Cache       *PersistentCache
	Sidecar     SidecarLoader
	Identity    func() string
	Diagnostics func(domain.Diagnostic)
}

func (s *StreamingService) TranscribeStream(ctx context.Context, episode domain.Episode, prepared audio.PreparedAudio, options StreamOptions, emit func(StreamCommit) error) error {
	if emit == nil {
		return fmt.Errorf("流式稳定提交回调不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateStreamPreparedIdentity(episode, prepared); err != nil {
		return err
	}
	if s.Sidecar != nil {
		if sidecar, ok := s.Sidecar(episode); ok {
			if _, err := validatedReplayTranscript(sidecar, prepared); err == nil {
				replay := ReplayStreamingTranscriber{Transcript: sidecar}
				return replay.TranscribeStream(ctx, episode, prepared, options, emit)
			}
		}
		s.diagnose(domain.Diagnostic{Code: "sidecar_unavailable", Message: "有效同名字幕不可用，继续尝试缓存或原生流式转写", EpisodeNumber: episode.EpisodeNumber})
	}
	identity := ""
	if s.Identity != nil {
		identity = s.Identity()
	} else if identified, ok := s.Backend.(interface{ Identity() string }); ok {
		identity = identified.Identity()
	}
	if s.Cache != nil && identity != "" {
		if cached, ok := s.Cache.Load(episode.VideoPath, identity); ok {
			if _, err := validatedReplayTranscript(cached, prepared); err == nil {
				replay := ReplayStreamingTranscriber{Transcript: cached}
				return replay.TranscribeStream(ctx, episode, prepared, options, emit)
			}
		}
	}
	if s.Backend == nil {
		return fmt.Errorf("%w: sidecar 与完整成功缓存均未命中", ErrStreamingBackendUnavailable)
	}
	if strings.TrimSpace(identity) == "" {
		return fmt.Errorf("%w: 缺少流式后端与模型身份", ErrStreamingBackendUnavailable)
	}

	collected := make([]domain.TranscriptItem, 0)
	lastRevision := uint64(0)
	lastThrough := 0.0
	finalSeen := false
	var complete domain.Transcript
	err := s.Backend.TranscribeStream(ctx, episode, prepared, options, func(commit StreamCommit) error {
		if finalSeen || commit.EpisodeNumber != episode.EpisodeNumber || commit.Revision == 0 || commit.Revision <= lastRevision ||
			!domain.Finite(commit.CommittedThroughSec) || commit.CommittedThroughSec < lastThrough || commit.CommittedThroughSec > prepared.DurationSec+1e-9 {
			return streamProtocolError("service_commit_order")
		}
		items, _, err := normalizeStreamItems(commit.Items, commit.CommittedThroughSec, false)
		if err != nil {
			return streamProtocolError("service_commit_items")
		}
		commit.Items = cloneStreamItems(items)
		collected = append(collected, cloneStreamItems(items)...)
		lastRevision = commit.Revision
		lastThrough = commit.CommittedThroughSec
		if commit.Final {
			if math.Abs(commit.CommittedThroughSec-prepared.DurationSec) > 1e-6 {
				return streamProtocolError("service_final_boundary")
			}
			complete = domain.Transcript{Source: identity, Items: cloneStreamItems(collected)}
			if prepared.MediaDurationSec <= 0 || audio.ValidateTimeline(prepared, prepared.MediaDurationSec, complete) != nil {
				return streamProtocolError("service_final_timeline")
			}
			finalSeen = true
		}
		return emit(cloneStreamCommit(commit))
	})
	if err != nil {
		return err
	}
	if !finalSeen {
		return streamProtocolError("final_commit_missing")
	}
	if s.Cache != nil && usable(complete) {
		if err := s.Cache.Store(episode.VideoPath, identity, complete); err != nil {
			return err
		}
	}
	return nil
}

func validateStreamPreparedIdentity(episode domain.Episode, prepared audio.PreparedAudio) error {
	if episode.EpisodeNumber < 1 || prepared.EpisodeNumber != episode.EpisodeNumber {
		return fmt.Errorf("预备分析音轨集数不匹配")
	}
	if err := prepared.Validate(); err != nil {
		return fmt.Errorf("预备分析音轨无效: %w", err)
	}
	if strings.TrimSpace(prepared.SourceVideoPath) == "" || !sameSourcePath(prepared.SourceVideoPath, episode.VideoPath) {
		return fmt.Errorf("预备分析音轨缺少源视频身份或与源视频不匹配")
	}
	return nil
}

func (s *StreamingService) diagnose(diagnostic domain.Diagnostic) {
	if s.Diagnostics != nil {
		s.Diagnostics(diagnostic)
	}
}
