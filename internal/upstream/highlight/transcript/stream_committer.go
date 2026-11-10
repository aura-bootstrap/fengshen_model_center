package transcript

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	DiagnosticStreamItemRejected        = "stream_stable_item_rejected"
	DiagnosticStreamZeroDurationSkipped = "stream_zero_duration_item_skipped"
)

type stableStreamItem struct {
	item   domain.TranscriptItem
	rounds int
}

// StablePrefixCommitter is the sole hypothesis-to-stable boundary. It compares
// only the uncommitted suffix, commits a contiguous stable prefix, and rejects
// any later attempt to rewrite committed history.
type StablePrefixCommitter struct {
	episode     int
	prepared    audio.PreparedAudio
	source      string
	options     StreamOptions
	diagnostics func(domain.Diagnostic)

	lastRevision     uint64
	committedThrough float64
	committed        []domain.TranscriptItem
	stable           []stableStreamItem
	final            bool
}

func NewStablePrefixCommitter(episode domain.Episode, prepared audio.PreparedAudio, source string, options StreamOptions, diagnostics func(domain.Diagnostic)) (*StablePrefixCommitter, error) {
	options = options.WithDefaults()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if episode.EpisodeNumber < 1 || prepared.EpisodeNumber != episode.EpisodeNumber || strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("流式稳定器身份无效")
	}
	if err := prepared.Validate(); err != nil {
		return nil, fmt.Errorf("流式稳定器预备音轨无效: %w", err)
	}
	return &StablePrefixCommitter{
		episode: episode.EpisodeNumber, prepared: prepared, source: source, options: options,
		diagnostics: diagnostics, committed: make([]domain.TranscriptItem, 0), stable: make([]stableStreamItem, 0),
	}, nil
}

// Push observes one complete decoder hypothesis. Every response must contain
// the already committed prefix verbatim followed by the current uncommitted
// suffix. A nil commit means more audio or stable rounds are required.
func (c *StablePrefixCommitter) Push(hypothesis StreamHypothesis) (*StreamCommit, error) {
	if c.final {
		return nil, streamProtocolError("message_after_final")
	}
	if hypothesis.EpisodeNumber != c.episode {
		return nil, streamProtocolError("episode_mismatch")
	}
	if hypothesis.Revision == 0 || hypothesis.Revision <= c.lastRevision {
		return nil, streamProtocolError("revision_order")
	}
	if !domain.Finite(hypothesis.CommittedThroughSec) || hypothesis.CommittedThroughSec < c.committedThrough ||
		hypothesis.CommittedThroughSec > c.prepared.DurationSec+1e-9 {
		return nil, streamProtocolError("committed_through_order")
	}
	if hypothesis.Final && math.Abs(hypothesis.CommittedThroughSec-c.prepared.DurationSec) > 1e-6 {
		return nil, streamProtocolError("final_media_boundary")
	}
	items, err := c.normalizeItems(hypothesis.Items, hypothesis.CommittedThroughSec, true)
	if err != nil {
		return nil, err
	}
	if len(items) < len(c.committed) {
		return nil, streamProtocolError("committed_prefix_missing")
	}
	for index, committed := range c.committed {
		if !reflect.DeepEqual(committed, items[index]) {
			return nil, streamProtocolError("committed_prefix_modified")
		}
	}
	uncommitted := cloneStreamItems(items[len(c.committed):])
	c.lastRevision = hypothesis.Revision
	previousThrough := c.committedThrough
	c.committedThrough = hypothesis.CommittedThroughSec

	if hypothesis.Final {
		commit := StreamCommit{
			EpisodeNumber: c.episode, Revision: hypothesis.Revision,
			CommittedThroughSec: c.committedThrough, Items: uncommitted, Final: true,
		}
		complete := domain.Transcript{Source: c.source, Items: append(cloneStreamItems(c.committed), cloneStreamItems(uncommitted)...)}
		if c.prepared.MediaDurationSec <= 0 || !domain.Finite(c.prepared.MediaDurationSec) {
			return nil, streamProtocolError("final_media_duration")
		}
		if err := audio.ValidateTimeline(c.prepared, c.prepared.MediaDurationSec, complete); err != nil {
			c.diagnose(DiagnosticStreamItemRejected, "完整流式转写时间轴校验失败")
			return nil, fmt.Errorf("完整流式转写时间轴无效: %w", err)
		}
		c.committed = append(c.committed, cloneStreamItems(uncommitted)...)
		c.stable = nil
		c.final = true
		return &commit, nil
	}

	nextStable := make([]stableStreamItem, len(uncommitted))
	for index, item := range uncommitted {
		rounds := 1
		if index < len(c.stable) && stableStreamItemEqual(c.stable[index].item, item, c.options.TimestampToleranceSec) {
			rounds = c.stable[index].rounds + 1
		}
		nextStable[index] = stableStreamItem{item: cloneStreamItem(item), rounds: rounds}
	}
	c.stable = nextStable

	commitCount := 0
	for commitCount < len(c.stable) {
		current := c.stable[commitCount]
		if current.rounds < c.options.StableRounds ||
			c.committedThrough+1e-9 < current.item.EndSec+c.options.FinalizeDelaySec {
			break
		}
		commitCount++
	}
	if commitCount == 0 && c.committedThrough == previousThrough {
		return nil, nil
	}
	newItems := make([]domain.TranscriptItem, 0, commitCount)
	for index := 0; index < commitCount; index++ {
		newItems = append(newItems, cloneStreamItem(c.stable[index].item))
	}
	c.committed = append(c.committed, cloneStreamItems(newItems)...)
	c.stable = c.stable[commitCount:]
	return &StreamCommit{
		EpisodeNumber: c.episode, Revision: hypothesis.Revision,
		CommittedThroughSec: c.committedThrough, Items: newItems, Final: false,
	}, nil
}

func stableStreamItemEqual(left, right domain.TranscriptItem, tolerance float64) bool {
	if normalizeWhisperText(left.Text) != normalizeWhisperText(right.Text) ||
		math.Abs(left.StartSec-right.StartSec) > tolerance || math.Abs(left.EndSec-right.EndSec) > tolerance ||
		len(left.Words) != len(right.Words) {
		return false
	}
	for index := range left.Words {
		leftWord, rightWord := left.Words[index], right.Words[index]
		if normalizeWhisperText(leftWord.Text) != normalizeWhisperText(rightWord.Text) ||
			math.Abs(leftWord.StartSec-rightWord.StartSec) > tolerance || math.Abs(leftWord.EndSec-rightWord.EndSec) > tolerance {
			return false
		}
	}
	return true
}

// normalizeStreamItems validates ordering, finite values, statement/word
// envelopes, probabilities, and the committed media boundary. Zero-duration
// text segments are skipped exactly like the existing Whisper JSON adapter;
// other malformed ranges fail closed instead of being repaired.
func normalizeStreamItems(items []domain.TranscriptItem, committedThrough float64, requireWordAlignment bool) ([]domain.TranscriptItem, int, error) {
	normalized := make([]domain.TranscriptItem, 0, len(items))
	previousStart := -1.0
	zeroDuration := 0
	for _, original := range items {
		item := cloneStreamItem(original)
		if !domain.Finite(item.StartSec) || !domain.Finite(item.EndSec) || item.StartSec < 0 || item.EndSec < item.StartSec {
			return nil, zeroDuration, fmt.Errorf("流式语句时间无效")
		}
		if item.EndSec == item.StartSec {
			if strings.TrimSpace(item.Text) != "" {
				zeroDuration++
			}
			continue
		}
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		if item.StartSec < previousStart || item.EndSec > committedThrough+1e-9 {
			return nil, zeroDuration, fmt.Errorf("流式语句排序或已提交媒体边界无效")
		}
		previousStart = item.StartSec
		if requireWordAlignment && len(item.Words) == 0 {
			return nil, zeroDuration, fmt.Errorf("流式 ASR 语句缺少词级时间")
		}
		previousWordEnd := -1.0
		var joined strings.Builder
		for _, word := range item.Words {
			if err := word.Validate(); err != nil || word.StartSec < item.StartSec || word.EndSec > item.EndSec || word.StartSec < previousWordEnd {
				return nil, zeroDuration, fmt.Errorf("流式词级时间或概率无效")
			}
			if strings.TrimSpace(word.Text) == "" {
				return nil, zeroDuration, fmt.Errorf("流式正文词 token 为空")
			}
			joined.WriteString(word.Text)
			previousWordEnd = word.EndSec
		}
		if len(item.Words) > 0 && normalizeWhisperText(joined.String()) != normalizeWhisperText(item.Text) {
			return nil, zeroDuration, fmt.Errorf("流式正文与词级对齐不一致")
		}
		if item.Probability != nil && (!domain.Finite(*item.Probability) || *item.Probability < 0 || *item.Probability > 1) {
			return nil, zeroDuration, fmt.Errorf("流式语句概率无效")
		}
		normalized = append(normalized, item)
	}
	return normalized, zeroDuration, nil
}

func (c *StablePrefixCommitter) normalizeItems(items []domain.TranscriptItem, through float64, requireWords bool) ([]domain.TranscriptItem, error) {
	normalized, skipped, err := normalizeStreamItems(items, through, requireWords)
	if skipped > 0 {
		c.diagnose(DiagnosticStreamZeroDurationSkipped, "已跳过零时长正文段")
	}
	if err != nil {
		c.diagnose(DiagnosticStreamItemRejected, "稳定语句时间、排序或词级对齐无效")
		return nil, streamProtocolError("invalid_stable_timeline")
	}
	return normalized, nil
}

func (c *StablePrefixCommitter) diagnose(code, message string) {
	if c.diagnostics != nil {
		c.diagnostics(domain.Diagnostic{Code: code, Message: message, EpisodeNumber: c.episode})
	}
}
