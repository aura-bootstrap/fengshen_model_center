package highlightclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

// BatchTranscriber also exposes Identity for the existing transcript cache.
type BatchTranscriber struct {
	client *Client
	vad    bool
}
type StreamingTranscriber struct {
	client *Client
	vad    bool
}

var _ transcript.Transcriber = (*BatchTranscriber)(nil)
var _ transcript.PreparedAudioTranscriber = (*BatchTranscriber)(nil)
var _ transcript.StreamingTranscriber = (*StreamingTranscriber)(nil)

func (c *Client) Transcriber(vad bool) *BatchTranscriber { return &BatchTranscriber{c, vad} }
func (c *Client) StreamingTranscriber(vad bool) *StreamingTranscriber {
	return &StreamingTranscriber{c, vad}
}
func identity(c *Client, component string, vad bool) string {
	b, e := c.binding("highlight.transcribe", component)
	if e != nil {
		return ""
	}
	raw, _ := json.Marshal(struct {
		Binding Binding
		VAD     bool
	}{b, vad})
	sum := sha256.Sum256(raw)
	return "model-center:" + component + ":" + hex.EncodeToString(sum[:])
}
func (t *BatchTranscriber) Identity() string     { return identity(t.client, "whisper-batch", t.vad) }
func (t *StreamingTranscriber) Identity() string { return identity(t.client, "whisper-stream", t.vad) }
func (t *BatchTranscriber) Transcribe(ctx context.Context, _ domain.Episode, _ transcript.ProgressFunc) (domain.Transcript, error) {
	if e := ctx.Err(); e != nil {
		return domain.Transcript{}, e
	}
	return domain.Transcript{}, ErrPreparedAudioRequired
}
func (t *BatchTranscriber) TranscribeWithAudio(ctx context.Context, ep domain.Episode, p audio.PreparedAudio, progress transcript.ProgressFunc) (domain.Transcript, error) {
	b, e := t.client.binding("highlight.transcribe", "whisper-batch")
	if e != nil {
		return domain.Transcript{}, e
	}
	id, e := requestID()
	if e != nil {
		return domain.Transcript{}, e
	}
	a, e := t.client.uploadPrepared(ctx, ep, p)
	if e != nil {
		return domain.Transcript{}, e
	}
	body := integration.RuntimeBody{Component: b.Component, Transcribe: &integration.RuntimeTranscribeInput{AssetID: a.ID, VAD: t.vad, EpisodeNumber: ep.EpisodeNumber}}
	result, e := t.client.run(ctx, b, id, body, func(v integration.RuntimeEvent) (bool, error) {
		if v.Commit != nil {
			return false, errResult
		}
		if progress != nil && v.Progress != nil {
			percent := 0
			if v.Progress.Total > 0 {
				percent = int(float64(v.Progress.Completed) / float64(v.Progress.Total) * 100)
			}
			progress(v.Progress.Stage, percent)
		}
		return false, nil
	}, func(r integration.RuntimeResult) error {
		if r.Stream != nil || r.Events != nil || r.Media != nil || r.Face != "" || len(r.Items) == 0 || !validItems(r.Items, 0, audioDuration(p)) {
			return errResult
		}
		items, e := mapItems(r.Items)
		if e != nil || (domain.Transcript{Source: t.Identity(), Items: items}).Validate() != nil {
			return errResult
		}
		return nil
	})
	if e != nil {
		return domain.Transcript{}, e
	}
	duration := audioDuration(p)
	if result.Stream != nil || result.Events != nil || result.Media != nil || result.Face != "" || len(result.Items) == 0 || !validItems(result.Items, 0, duration) {
		return domain.Transcript{}, errResult
	}
	items, e := mapItems(result.Items)
	if e != nil {
		return domain.Transcript{}, e
	}
	out := domain.Transcript{Source: t.Identity(), Items: items}
	if out.Source == "" || out.Validate() != nil {
		return domain.Transcript{}, errResult
	}
	return out, nil
}
func audioDuration(p audio.PreparedAudio) float64 {
	d := p.DurationSec
	if p.MediaDurationSec > 0 && p.MediaDurationSec < d {
		d = p.MediaDurationSec
	}
	return d
}
func mapItems(items []integration.RuntimeTranscriptItem) ([]domain.TranscriptItem, error) {
	raw, e := json.Marshal(items)
	if e != nil {
		return nil, errResult
	}
	var out []domain.TranscriptItem
	if json.Unmarshal(raw, &out) != nil {
		return nil, errResult
	}
	return out, nil
}
func validItems(items []integration.RuntimeTranscriptItem, start, end float64) bool {
	if len(items) > 16384 {
		return false
	}
	last := start
	for _, i := range items {
		if !domain.ValidRange(i.StartSec, i.EndSec) || i.StartSec < last || i.EndSec > end || strings.TrimSpace(i.Text) == "" || !validProbability(i.Probability) {
			return false
		}
		last = i.EndSec
		wordEnd := i.StartSec
		for _, w := range i.Words {
			if !domain.ValidRange(w.StartSec, w.EndSec) || w.StartSec < wordEnd || w.EndSec > i.EndSec || strings.TrimSpace(w.Text) == "" || !validProbability(w.Probability) {
				return false
			}
			wordEnd = w.EndSec
		}
	}
	return true
}
func validProbability(p *float64) bool { return p == nil || domain.Finite(*p) && *p >= 0 && *p <= 1 }

// TranscribeStream normalizes legacy zero options before freezing typed options.
// Zero tolerance and finalize delay in otherwise populated options remain explicit.
func (t *StreamingTranscriber) TranscribeStream(ctx context.Context, ep domain.Episode, p audio.PreparedAudio, options transcript.StreamOptions, emit func(transcript.StreamCommit) error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if options == (transcript.StreamOptions{}) {
		options = transcript.DefaultStreamOptions()
	}
	options = options.WithDefaults()
	if options.Validate() != nil {
		return errInput
	}
	if emit == nil {
		return errInput
	}
	b, e := t.client.binding("highlight.transcribe", "whisper-stream")
	if e != nil {
		return e
	}
	id, e := requestID()
	if e != nil {
		return e
	}
	a, e := t.client.uploadPrepared(ctx, ep, p)
	if e != nil {
		return e
	}
	frozenOptions := integration.RuntimeStreamOptions(options)
	body := integration.RuntimeBody{Component: b.Component, Transcribe: &integration.RuntimeTranscribeInput{AssetID: a.ID, Stream: true, VAD: t.vad, EpisodeNumber: ep.EpisodeNumber, Options: &frozenOptions}}
	var last *integration.RuntimeStreamCommit
	var all []integration.RuntimeTranscriptItem
	var sequence uint64
	matchesCumulative := func(stream *integration.RuntimeStreamCommit) bool {
		if last == nil || !last.Final || stream == nil {
			return false
		}
		cumulative := *last
		cumulative.Items = all
		return reflect.DeepEqual(&cumulative, stream)
	}
	result, e := t.client.run(ctx, b, id, body, func(v integration.RuntimeEvent) (bool, error) {
		if sequence == ^uint64(0) || v.Sequence != sequence+1 {
			return false, errResult
		}
		sequence = v.Sequence
		if v.Commit == nil {
			return false, nil
		}
		commit := v.Commit
		if commit.EpisodeNumber != ep.EpisodeNumber || commit.Revision == 0 || !domain.Finite(commit.CommittedThroughSec) || commit.CommittedThroughSec < 0 || commit.CommittedThroughSec > p.DurationSec || !validItems(commit.Items, 0, commit.CommittedThroughSec) {
			return false, errResult
		}
		if last != nil && (last.Final || commit.Revision <= last.Revision || commit.CommittedThroughSec < last.CommittedThroughSec || !validItems(commit.Items, last.CommittedThroughSec, commit.CommittedThroughSec)) {
			return false, errResult
		}
		if commit.Final && math.Abs(commit.CommittedThroughSec-p.DurationSec) > 1e-9 {
			return false, errResult
		}
		raw, e := json.Marshal(commit)
		if e != nil {
			return false, errResult
		}
		var owned integration.RuntimeStreamCommit
		if json.Unmarshal(raw, &owned) != nil {
			return false, errResult
		}
		last = &owned
		all = append(all, owned.Items...)
		if len(all) > 16384 || !validItems(all, 0, p.DurationSec) {
			return false, errResult
		}
		items, e := mapItems(commit.Items)
		if e != nil {
			return false, e
		}
		e = emit(transcript.StreamCommit{EpisodeNumber: commit.EpisodeNumber, Revision: commit.Revision, CommittedThroughSec: commit.CommittedThroughSec, Items: items, Final: commit.Final})
		return e != nil, e
	}, func(r integration.RuntimeResult) error {
		if !matchesCumulative(r.Stream) || r.Items != nil || r.Events != nil || r.Media != nil || r.Face != "" || len(all) == 0 || !validItems(all, 0, audioDuration(p)) {
			return errResult
		}
		items, e := mapItems(all)
		if e != nil || (domain.Transcript{Source: t.Identity(), Items: items}).Validate() != nil {
			return errResult
		}
		return nil
	})
	if e != nil {
		return e
	}
	// Result.Stream must match the observed final identity and all ordered SSE items.
	// The cumulative result cannot repair an SSE gap.
	if !matchesCumulative(result.Stream) || result.Items != nil || result.Events != nil || result.Media != nil || result.Face != "" || len(all) == 0 || !validItems(all, 0, audioDuration(p)) {
		return errResult
	}
	items, e := mapItems(all)
	if e != nil {
		return e
	}
	if (domain.Transcript{Source: t.Identity(), Items: items}).Validate() != nil {
		return errResult
	}
	return nil
}
