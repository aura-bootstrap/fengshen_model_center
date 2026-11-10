package audio

import (
	"errors"
	"fmt"
	"math"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	DefaultMaxTimelineDriftSec = 0.25
	DefaultTimestampTolerance  = 0.05
)

// TimelineOptions controls container/WAV drift and small timestamp rounding
// tolerances. Zero values select conservative defaults.
type TimelineOptions struct {
	MaxDriftSec        float64
	TimestampTolerance float64
}

func (o TimelineOptions) normalized() (TimelineOptions, error) {
	if o.MaxDriftSec == 0 {
		o.MaxDriftSec = DefaultMaxTimelineDriftSec
	}
	if o.TimestampTolerance == 0 {
		o.TimestampTolerance = DefaultTimestampTolerance
	}
	if !positiveFinite(o.MaxDriftSec) || o.TimestampTolerance < 0 || math.IsNaN(o.TimestampTolerance) || math.IsInf(o.TimestampTolerance, 0) {
		return TimelineOptions{}, fmt.Errorf("%w: invalid timeline tolerances", ErrInvalidRequest)
	}
	return o, nil
}

type timelineRange struct {
	startSec float64
	endSec   float64
}

// ValidateTimeline checks WAV/container agreement and every transcript range.
// Empty PCM and unreliable no-speech claims are errors, never silence evidence.
func ValidateTimeline(prepared PreparedAudio, mediaDurationSec float64, transcript domain.Transcript, options ...TimelineOptions) error {
	ranges := make([]timelineRange, 0, len(transcript.Items))
	for _, item := range transcript.Items {
		ranges = append(ranges, timelineRange{startSec: item.StartSec, endSec: item.EndSec})
	}
	if err := transcript.Validate(); err != nil {
		return fmt.Errorf("%w: invalid transcript timeline: %v", ErrTimeline, err)
	}
	return validateTimelineRanges(prepared, mediaDurationSec, ranges, options...)
}

// ValidateSpeechUnitTimeline applies the same checks to stable speech units.
func ValidateSpeechUnitTimeline(prepared PreparedAudio, mediaDurationSec float64, units []domain.SpeechUnit, options ...TimelineOptions) error {
	ranges := make([]timelineRange, 0, len(units))
	for _, unit := range units {
		ranges = append(ranges, timelineRange{startSec: unit.StartSec, endSec: unit.EndSec})
	}
	return validateTimelineRanges(prepared, mediaDurationSec, ranges, options...)
}

func validateTimelineRanges(prepared PreparedAudio, mediaDurationSec float64, ranges []timelineRange, options ...TimelineOptions) error {
	if len(options) > 1 {
		return fmt.Errorf("%w: multiple timeline options", ErrInvalidRequest)
	}
	option := TimelineOptions{}
	if len(options) == 1 {
		option = options[0]
	}
	option, err := option.normalized()
	if err != nil {
		return err
	}
	if err := prepared.Validate(); err != nil {
		if errorsIsEmptyAudio(err) {
			return err
		}
		return fmt.Errorf("%w: invalid prepared audio: %v", ErrTimeline, err)
	}
	if !positiveFinite(mediaDurationSec) {
		return fmt.Errorf("%w: ffprobe media duration must be finite and positive", ErrTimeline)
	}
	if math.Abs(prepared.DurationSec-mediaDurationSec) > option.MaxDriftSec {
		return fmt.Errorf("%w: WAV and ffprobe durations differ by more than %.3fs", ErrTimeline, option.MaxDriftSec)
	}
	if prepared.MediaDurationSec > 0 && math.Abs(prepared.MediaDurationSec-mediaDurationSec) > option.MaxDriftSec {
		return fmt.Errorf("%w: prepared and requested media durations differ", ErrTimeline)
	}

	previousStart := -1.0
	for _, current := range ranges {
		if current.startSec < 0 || math.IsNaN(current.startSec) || math.IsInf(current.startSec, 0) ||
			current.endSec <= current.startSec || math.IsNaN(current.endSec) || math.IsInf(current.endSec, 0) {
			return fmt.Errorf("%w: invalid statement range", ErrTimeline)
		}
		if current.startSec+option.TimestampTolerance < previousStart {
			return fmt.Errorf("%w: statement start times are not stable", ErrTimeline)
		}
		previousStart = current.startSec
		if current.endSec > prepared.DurationSec+option.TimestampTolerance || current.endSec > mediaDurationSec+option.TimestampTolerance {
			return fmt.Errorf("%w: statement exceeds the audio/media timeline", ErrTimeline)
		}
	}
	return nil
}

func errorsIsEmptyAudio(err error) bool {
	return errors.Is(err, ErrEmptyAudio)
}
