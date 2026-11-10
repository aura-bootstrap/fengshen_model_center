package audio

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

const (
	// FrameDurationSec is the fixed analysis frame and hop duration.
	FrameDurationSec = 0.020
	// SilenceDBFS is a finite floor used instead of -Inf for digital silence.
	SilenceDBFS        = -120.0
	DefaultMaxFrames   = 1_000_000
	frameCheckInterval = 256
)

// PCMFrame contains process-local signal measurements. It is deliberately not
// serializable: audio_features_v1 contains window aggregates, never frame,
// sample, autocorrelation, or spectral sequences.
type PCMFrame struct {
	StartSample      int     `json:"-"`
	EndSample        int     `json:"-"`
	StartSec         float64 `json:"-"`
	EndSec           float64 `json:"-"`
	RMS              float64 `json:"-"`
	DBFS             float64 `json:"-"`
	PeakDBFS         float64 `json:"-"`
	ZeroCrossingRate float64 `json:"-"`
	Speech           bool    `json:"-"`

	PitchValid      bool    `json:"-"`
	PitchHz         float64 `json:"-"`
	PitchConfidence float64 `json:"-"`
	pitchAnalyzed   bool    `json:"-"`
	pitch           pitchObservation
}

func (PCMFrame) MarshalJSON() ([]byte, error) { return nil, ErrLocalMediaSerialization }

// FramingOptions bounds memory and processing without changing the fixed 20 ms
// frame/hop semantics.
type FramingOptions struct {
	MaxFrames int
}

func (o FramingOptions) normalized() (FramingOptions, error) {
	if o.MaxFrames == 0 {
		o.MaxFrames = DefaultMaxFrames
	}
	if o.MaxFrames < 1 || o.MaxFrames > DefaultMaxFrames {
		return FramingOptions{}, fmt.Errorf("%w: maximum frame count must be between 1 and %d", ErrInvalidRequest, DefaultMaxFrames)
	}
	return o, nil
}

// FramePCM computes RMS, finite dBFS, peak dBFS, and zero-crossing rate for
// every non-empty 20 ms frame. The final short frame is measured over its real
// samples (not zero padded), and its end remains on the exact PCM timeline.
func FramePCM(ctx context.Context, pcm PCM, options ...FramingOptions) ([]PCMFrame, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("%w: multiple framing options", ErrInvalidRequest)
	}
	option := FramingOptions{}
	if len(options) == 1 {
		option = options[0]
	}
	option, err := option.normalized()
	if err != nil {
		return nil, err
	}
	if err := pcm.Format.Validate(); err != nil {
		return nil, err
	}
	if len(pcm.Samples) == 0 {
		return nil, ErrEmptyAudio
	}
	sampleRate := pcm.Format.SampleRate
	duration := float64(len(pcm.Samples)) / float64(sampleRate)
	if pcm.DurationSec != 0 && (!positiveFinite(pcm.DurationSec) || math.Abs(pcm.DurationSec-duration) > 1/float64(sampleRate)) {
		return nil, fmt.Errorf("%w: PCM duration does not match its sample count", ErrTimeline)
	}
	frameSamples := int(math.Round(float64(sampleRate) * FrameDurationSec))
	if frameSamples < 1 {
		return nil, fmt.Errorf("%w: invalid PCM frame size", ErrInvalidRequest)
	}
	frameCount := 1 + (len(pcm.Samples)-1)/frameSamples
	if frameCount > option.MaxFrames {
		return nil, fmt.Errorf("%w: %d frames exceed %d", ErrResourceLimit, frameCount, option.MaxFrames)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	frames := make([]PCMFrame, 0, frameCount)
	for start := 0; start < len(pcm.Samples); start += frameSamples {
		if len(frames)%frameCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		end := start + frameSamples
		if end > len(pcm.Samples) {
			end = len(pcm.Samples)
		}
		var sumSquares, peak float64
		crossings := 0
		for index := start; index < end; index++ {
			value := float64(pcm.Samples[index]) / 32768.0
			sumSquares += value * value
			absolute := math.Abs(value)
			if absolute > peak {
				peak = absolute
			}
			if index > start && (pcm.Samples[index] < 0) != (pcm.Samples[index-1] < 0) {
				crossings++
			}
		}
		count := end - start
		rms := math.Sqrt(sumSquares / float64(count))
		zcr := 0.0
		if count > 1 {
			zcr = float64(crossings) / float64(count-1)
		}
		frames = append(frames, PCMFrame{
			StartSample: start, EndSample: end,
			StartSec: float64(start) / float64(sampleRate), EndSec: float64(end) / float64(sampleRate),
			RMS: rms, DBFS: amplitudeDBFS(rms), PeakDBFS: amplitudeDBFS(peak), ZeroCrossingRate: zcr,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return frames, nil
}

func amplitudeDBFS(amplitude float64) float64 {
	if amplitude <= 0 {
		return SilenceDBFS
	}
	value := 20 * math.Log10(amplitude)
	if value < SilenceDBFS {
		return SilenceDBFS
	}
	if value > 0 {
		// Only the negative full-scale integer can normalize to exactly 1. Keep
		// the wire/analyzer invariant defensive against future format changes.
		return 0
	}
	return value
}

func validateFrames(frames []PCMFrame) error {
	if len(frames) == 0 {
		return ErrEmptyAudio
	}
	if len(frames) > DefaultMaxFrames {
		return fmt.Errorf("%w: too many PCM frames", ErrResourceLimit)
	}
	previousEnd := -1.0
	for index, frame := range frames {
		values := []float64{frame.StartSec, frame.EndSec, frame.RMS, frame.DBFS, frame.PeakDBFS, frame.ZeroCrossingRate}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("%w: non-finite PCM frame measurement", ErrAnalysis)
			}
		}
		if frame.StartSec < 0 || frame.EndSec <= frame.StartSec || frame.RMS < 0 || frame.RMS > 1 ||
			frame.DBFS < SilenceDBFS || frame.DBFS > 0 || frame.PeakDBFS < SilenceDBFS || frame.PeakDBFS > 0 ||
			frame.ZeroCrossingRate < 0 || frame.ZeroCrossingRate > 1 {
			return fmt.Errorf("%w: invalid PCM frame measurement", ErrAnalysis)
		}
		if index > 0 && (frame.StartSec < previousEnd || math.Abs(frame.StartSec-previousEnd) > 1e-9) {
			return fmt.Errorf("%w: PCM frames must be sorted and contiguous", ErrTimeline)
		}
		previousEnd = frame.EndSec
	}
	return nil
}

var _ json.Marshaler = PCMFrame{}
