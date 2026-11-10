package audio

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	// OverlapPossible is the only overlap classification emitted by the local
	// detector. It is not a diarization result and carries no speaker identity.
	OverlapPossible    = "overlap_possible"
	DefaultMaxOverlaps = 4096
)

var ErrOverlapDetectorUnavailable = errors.New("overlap detector pitch evidence is unavailable")

// OverlapDetector is injectable so a deployment can conservatively degrade when
// multi-pitch analysis is unavailable without blocking the audio/text chain.
type OverlapDetector interface {
	DetectOverlap(context.Context, []PCMFrame) ([]domain.OverlapInterval, error)
}

// ConservativeOverlapDetector requires sustained, non-harmonic multi-pitch
// evidence on speech frames. It intentionally misses ambiguous overlap rather
// than turning harmonics, music, or a single fundamental into speaker claims.
type ConservativeOverlapDetector struct {
	MinConsecutiveFrames int
	MinConfidence        float64
}

func defaultConservativeOverlapDetector() ConservativeOverlapDetector {
	return ConservativeOverlapDetector{MinConsecutiveFrames: 3, MinConfidence: 0.60}
}

func (d ConservativeOverlapDetector) normalized() (ConservativeOverlapDetector, error) {
	defaults := defaultConservativeOverlapDetector()
	if d.MinConsecutiveFrames == 0 {
		d.MinConsecutiveFrames = defaults.MinConsecutiveFrames
	}
	if d.MinConfidence == 0 {
		d.MinConfidence = defaults.MinConfidence
	}
	return d.validated()
}

func (d ConservativeOverlapDetector) validated() (ConservativeOverlapDetector, error) {
	if d.MinConsecutiveFrames < 1 || d.MinConsecutiveFrames > 100 ||
		!finite(d.MinConfidence) || d.MinConfidence <= 0 || d.MinConfidence > 1 {
		return ConservativeOverlapDetector{}, fmt.Errorf("%w: invalid conservative overlap options", ErrInvalidRequest)
	}
	return d, nil
}

func (d ConservativeOverlapDetector) DetectOverlap(ctx context.Context, frames []PCMFrame) ([]domain.OverlapInterval, error) {
	option, err := d.normalized()
	if err != nil {
		return nil, err
	}
	if err := validateFrames(frames); err != nil {
		return nil, err
	}
	type run struct {
		start      int
		end        int
		confidence float64
		count      int
	}
	runs := make([]run, 0)
	current := run{start: -1}
	flush := func() {
		if current.start >= 0 && current.count >= option.MinConsecutiveFrames {
			runs = append(runs, current)
		}
		current = run{start: -1}
	}
	for index, frame := range frames {
		if index%frameCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if frame.Speech && !frame.pitchAnalyzed {
			return nil, ErrOverlapDetectorUnavailable
		}
		score, possible := multiPitchConfidence(frame, option.MinConfidence)
		if !possible {
			flush()
			continue
		}
		if current.start < 0 {
			current = run{start: index}
		}
		current.end = index
		current.count++
		current.confidence += score
	}
	flush()
	if len(runs) > DefaultMaxOverlaps {
		return nil, fmt.Errorf("%w: too many overlap intervals", ErrResourceLimit)
	}
	overlaps := make([]domain.OverlapInterval, 0, len(runs))
	for _, current := range runs {
		confidence := current.confidence / float64(current.count)
		if confidence > 0.95 {
			confidence = 0.95
		}
		interval := domain.OverlapInterval{
			AudioInterval: domain.AudioInterval{StartSec: frames[current.start].StartSec, EndSec: frames[current.end].EndSec},
			Confidence:    confidence,
		}
		if err := interval.Validate(); err != nil || !finite(confidence) {
			return nil, fmt.Errorf("%w: invalid overlap detector output", ErrAnalysis)
		}
		overlaps = append(overlaps, interval)
	}
	return overlaps, nil
}

func multiPitchConfidence(frame PCMFrame, minimum float64) (float64, bool) {
	if !frame.Speech || !frame.PitchValid || !frame.pitchAnalyzed || !frame.pitch.hasMultiPitch {
		return 0, false
	}
	first, second := frame.pitch.first, frame.pitch.second
	if first.confidence < minimum || second.confidence < minimum ||
		!finite(first.hz) || !finite(second.hz) || first.hz <= 0 || second.hz <= 0 || harmonicPitchPair(first.hz, second.hz) {
		return 0, false
	}
	// Confidence is deliberately discounted: multi-pitch is only a conservative
	// overlap_possible clue, never proof of a particular number of speakers.
	score := 0.75 * math.Min(first.confidence, second.confidence)
	return clampUnit(score), score > 0
}
