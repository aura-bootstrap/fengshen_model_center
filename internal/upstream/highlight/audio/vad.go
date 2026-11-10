package audio

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	EnergyVADBackend       = "energy_v1"
	DefaultMaxIntervals    = 65_536
	defaultNoisePercentile = 20.0
)

// AnalysisCoverage describes the exact timeline processed by VAD. Complete
// means every supplied frame is contiguous; NoSpeechEvidenceFor additionally
// requires coverage of the complete media duration.
type AnalysisCoverage struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
	Complete bool    `json:"complete"`
}

// VADOptions controls deterministic energy thresholds and state hysteresis.
// Zero values select conservative defaults.
type VADOptions struct {
	NoiseFloorPercentile float64
	EnterOffsetDB        float64
	ExitOffsetDB         float64
	AbsoluteEnterDBFS    float64
	AbsoluteExitDBFS     float64
	EnterFrames          int
	ExitFrames           int
	MinSpeechDurationSec float64
	MergeSpeechGapSec    float64
	PauseThresholdSec    float64
}

func defaultVADOptions() VADOptions {
	return VADOptions{
		NoiseFloorPercentile: defaultNoisePercentile,
		EnterOffsetDB:        10,
		ExitOffsetDB:         6,
		AbsoluteEnterDBFS:    -50,
		AbsoluteExitDBFS:     -55,
		EnterFrames:          2,
		ExitFrames:           3,
		MinSpeechDurationSec: 0.06,
		MergeSpeechGapSec:    0.20,
		PauseThresholdSec:    0.35,
	}
}

// normalized retains the legacy per-field zero-value defaults used by Analyze,
// DetectVAD, and existing callers. AnalysisOptions uses validated directly so
// explicit zero thresholds in a complete new configuration remain meaningful.
func (o VADOptions) normalized() (VADOptions, error) {
	defaults := defaultVADOptions()
	if o.NoiseFloorPercentile == 0 {
		o.NoiseFloorPercentile = defaults.NoiseFloorPercentile
	}
	if o.EnterOffsetDB == 0 {
		o.EnterOffsetDB = defaults.EnterOffsetDB
	}
	if o.ExitOffsetDB == 0 {
		o.ExitOffsetDB = defaults.ExitOffsetDB
	}
	if o.AbsoluteEnterDBFS == 0 {
		o.AbsoluteEnterDBFS = defaults.AbsoluteEnterDBFS
	}
	if o.AbsoluteExitDBFS == 0 {
		o.AbsoluteExitDBFS = defaults.AbsoluteExitDBFS
	}
	if o.EnterFrames == 0 {
		o.EnterFrames = defaults.EnterFrames
	}
	if o.ExitFrames == 0 {
		o.ExitFrames = defaults.ExitFrames
	}
	if o.MinSpeechDurationSec == 0 {
		o.MinSpeechDurationSec = defaults.MinSpeechDurationSec
	}
	if o.MergeSpeechGapSec == 0 {
		o.MergeSpeechGapSec = defaults.MergeSpeechGapSec
	}
	if o.PauseThresholdSec == 0 {
		o.PauseThresholdSec = defaults.PauseThresholdSec
	}
	return o.validated()
}

func (o VADOptions) validated() (VADOptions, error) {
	finiteValues := []float64{
		o.NoiseFloorPercentile, o.EnterOffsetDB, o.ExitOffsetDB,
		o.AbsoluteEnterDBFS, o.AbsoluteExitDBFS, o.MinSpeechDurationSec,
		o.MergeSpeechGapSec, o.PauseThresholdSec,
	}
	for _, value := range finiteValues {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return VADOptions{}, fmt.Errorf("%w: VAD options must be finite", ErrInvalidRequest)
		}
	}
	if o.NoiseFloorPercentile <= 0 || o.NoiseFloorPercentile >= 100 ||
		o.EnterOffsetDB < 0 || o.ExitOffsetDB < 0 || o.ExitOffsetDB > o.EnterOffsetDB ||
		o.AbsoluteEnterDBFS < SilenceDBFS || o.AbsoluteEnterDBFS > 0 ||
		o.AbsoluteExitDBFS < SilenceDBFS || o.AbsoluteExitDBFS > o.AbsoluteEnterDBFS ||
		o.EnterOffsetDB > 120 || o.ExitOffsetDB > 120 ||
		o.EnterFrames < 1 || o.ExitFrames < 1 || o.EnterFrames > 1000 || o.ExitFrames > 1000 ||
		o.MinSpeechDurationSec < 0 || o.MinSpeechDurationSec > 86400 ||
		o.MergeSpeechGapSec < 0 || o.MergeSpeechGapSec > 86400 ||
		o.PauseThresholdSec < 0 || o.PauseThresholdSec > 86400 {
		return VADOptions{}, fmt.Errorf("%w: invalid VAD thresholds or hysteresis", ErrInvalidRequest)
	}
	return o, nil
}

// VADResult retains local frames for metric projection and exposes only compact
// timeline evidence through AudioAnalysis.
type VADResult struct {
	Frames             []PCMFrame             `json:"-"`
	Speech             []domain.AudioInterval `json:"speech"`
	Silence            []domain.AudioInterval `json:"silence"`
	NoiseFloorDBFS     float64                `json:"noise_floor_dbfs"`
	EnterThresholdDBFS float64                `json:"enter_threshold_dbfs"`
	ExitThresholdDBFS  float64                `json:"exit_threshold_dbfs"`
	Reliable           bool                   `json:"reliable"`
	Coverage           AnalysisCoverage       `json:"coverage"`
}

// EstimateNoiseFloor returns the requested percentile of finite frame dBFS.
func EstimateNoiseFloor(frames []PCMFrame, percentile ...float64) (float64, error) {
	if len(percentile) > 1 {
		return 0, fmt.Errorf("%w: multiple noise percentiles", ErrInvalidRequest)
	}
	value := defaultNoisePercentile
	if len(percentile) == 1 {
		value = percentile[0]
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value >= 100 {
		return 0, fmt.Errorf("%w: noise percentile must be between 0 and 100", ErrInvalidRequest)
	}
	if err := validateFrames(frames); err != nil {
		return 0, err
	}
	return noiseFloorPercentile(frames, value), nil
}

func noiseFloorPercentile(frames []PCMFrame, percentile float64) float64 {
	levels := make([]float64, len(frames))
	for index, frame := range frames {
		levels[index] = frame.DBFS
	}
	sort.Float64s(levels)
	return percentileLinear(levels, percentile)
}

// DetectVAD applies separate enter/exit thresholds and frame hysteresis, drops
// short isolated bursts, merges speech across short gaps, and emits a complete,
// sorted speech/silence partition of the analyzed timeline.
func DetectVAD(ctx context.Context, frames []PCMFrame, options ...VADOptions) (VADResult, error) {
	if len(options) > 1 {
		return VADResult{}, fmt.Errorf("%w: multiple VAD options", ErrInvalidRequest)
	}
	option := VADOptions{}
	if len(options) == 1 {
		option = options[0]
	}
	option, err := option.normalized()
	if err != nil {
		return VADResult{}, err
	}
	return detectVADValidated(ctx, frames, option)
}

// detectVADValidated accepts a complete, already validated configuration. It
// keeps explicit zero thresholds intact for AnalysisOptions while DetectVAD
// retains its legacy per-field zero defaults.
func detectVADValidated(ctx context.Context, frames []PCMFrame, option VADOptions) (VADResult, error) {
	if err := validateFrames(frames); err != nil {
		return VADResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return VADResult{}, err
	}
	noiseFloor := noiseFloorPercentile(frames, option.NoiseFloorPercentile)
	enterThreshold := math.Max(option.AbsoluteEnterDBFS, noiseFloor+option.EnterOffsetDB)
	exitThreshold := math.Max(option.AbsoluteExitDBFS, noiseFloor+option.ExitOffsetDB)
	if exitThreshold > enterThreshold {
		exitThreshold = enterThreshold
	}
	if !finite(noiseFloor) || !finite(enterThreshold) || !finite(exitThreshold) {
		return VADResult{}, fmt.Errorf("%w: non-finite VAD threshold", ErrAnalysis)
	}

	localFrames := append([]PCMFrame(nil), frames...)
	rawSpeech := make([]domain.AudioInterval, 0)
	active := false
	aboveRun, belowRun := 0, 0
	runStart := 0.0
	activeStart := 0.0
	for index, frame := range localFrames {
		if index%frameCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return VADResult{}, err
			}
		}
		if !active {
			if frame.DBFS >= enterThreshold {
				if aboveRun == 0 {
					runStart = frame.StartSec
				}
				aboveRun++
				if aboveRun >= option.EnterFrames {
					active = true
					activeStart = runStart
					aboveRun = 0
					belowRun = 0
				}
			} else {
				aboveRun = 0
			}
			continue
		}

		if frame.DBFS < exitThreshold {
			belowRun++
			if belowRun >= option.ExitFrames {
				rawSpeech = append(rawSpeech, domain.AudioInterval{StartSec: activeStart, EndSec: frame.EndSec})
				active = false
				belowRun = 0
				aboveRun = 0
			}
		} else {
			belowRun = 0
		}
	}
	if active {
		rawSpeech = append(rawSpeech, domain.AudioInterval{StartSec: activeStart, EndSec: localFrames[len(localFrames)-1].EndSec})
	}
	if len(rawSpeech) > DefaultMaxIntervals {
		return VADResult{}, fmt.Errorf("%w: too many raw speech intervals", ErrResourceLimit)
	}

	speech := make([]domain.AudioInterval, 0, len(rawSpeech))
	for _, interval := range rawSpeech {
		if interval.EndSec-interval.StartSec+1e-9 < option.MinSpeechDurationSec {
			continue
		}
		if len(speech) > 0 {
			last := &speech[len(speech)-1]
			gap := interval.StartSec - last.EndSec
			if gap >= 0 && gap <= option.MergeSpeechGapSec {
				last.EndSec = interval.EndSec
				continue
			}
		}
		speech = append(speech, interval)
	}
	if len(speech) > DefaultMaxIntervals {
		return VADResult{}, fmt.Errorf("%w: too many speech intervals", ErrResourceLimit)
	}
	for index := range localFrames {
		localFrames[index].Speech = false
		for _, interval := range speech {
			if localFrames[index].EndSec > interval.StartSec && localFrames[index].StartSec < interval.EndSec {
				localFrames[index].Speech = true
				break
			}
		}
	}

	start := localFrames[0].StartSec
	end := localFrames[len(localFrames)-1].EndSec
	silence := complementIntervals(speech, start, end)
	if len(silence) > DefaultMaxIntervals {
		return VADResult{}, fmt.Errorf("%w: too many silence intervals", ErrResourceLimit)
	}
	return VADResult{
		Frames: localFrames, Speech: speech, Silence: silence,
		NoiseFloorDBFS: noiseFloor, EnterThresholdDBFS: enterThreshold, ExitThresholdDBFS: exitThreshold,
		Reliable: true, Coverage: AnalysisCoverage{StartSec: start, EndSec: end, Complete: true},
	}, nil
}

func complementIntervals(speech []domain.AudioInterval, start, end float64) []domain.AudioInterval {
	silence := make([]domain.AudioInterval, 0, len(speech)+1)
	cursor := start
	for _, interval := range speech {
		if interval.StartSec > cursor {
			silence = append(silence, domain.AudioInterval{StartSec: cursor, EndSec: interval.StartSec})
		}
		if interval.EndSec > cursor {
			cursor = interval.EndSec
		}
	}
	if end > cursor {
		silence = append(silence, domain.AudioInterval{StartSec: cursor, EndSec: end})
	}
	return silence
}

// DerivePauses creates a pause exactly at every qualifying gap between adjacent
// speech intervals. Threshold equality is included; zero-length gaps are not
// intervals and overlapping/unsorted speech is rejected rather than repaired.
func DerivePauses(speech []domain.AudioInterval, thresholdSec float64) ([]domain.AudioInterval, error) {
	if math.IsNaN(thresholdSec) || math.IsInf(thresholdSec, 0) || thresholdSec < 0 {
		return nil, fmt.Errorf("%w: pause threshold must be finite and non-negative", ErrInvalidRequest)
	}
	if len(speech) > DefaultMaxIntervals {
		return nil, fmt.Errorf("%w: too many speech intervals", ErrResourceLimit)
	}
	pauses := make([]domain.AudioInterval, 0)
	for index, interval := range speech {
		if err := interval.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid speech interval", ErrTimeline)
		}
		if index == 0 {
			continue
		}
		previous := speech[index-1]
		if interval.StartSec < previous.EndSec {
			return nil, fmt.Errorf("%w: speech intervals overlap or are unsorted", ErrTimeline)
		}
		gap := interval.StartSec - previous.EndSec
		if gap > 0 && gap >= thresholdSec {
			pauses = append(pauses, domain.AudioInterval{StartSec: previous.EndSec, EndSec: interval.StartSec})
		}
	}
	return pauses, nil
}

// NoSpeechEvidenceFor is the sole evidence gate in this package. Empty ASR text
// is not an input and can never manufacture evidence. Failed, partial, invalid,
// unreliable, or speech-bearing analysis returns nil.
func NoSpeechEvidenceFor(analysis AudioAnalysis) *domain.NoSpeechEvidence {
	if !positiveFinite(analysis.DurationSec) || !analysis.VAD.Reliable ||
		strings.TrimSpace(analysis.VAD.Backend) == "" || analysis.VAD.Backend != strings.TrimSpace(analysis.VAD.Backend) ||
		!analysis.Coverage.Complete ||
		analysis.Coverage.StartSec != 0 || math.Abs(analysis.Coverage.EndSec-analysis.DurationSec) > 1e-9 ||
		len(analysis.Speech) != 0 || len(analysis.Frames) == 0 {
		return nil
	}
	if validateFrames(analysis.Frames) != nil || analysis.Frames[0].StartSec != 0 ||
		math.Abs(analysis.Frames[len(analysis.Frames)-1].EndSec-analysis.DurationSec) > 1e-9 {
		return nil
	}
	for _, frame := range analysis.Frames {
		if frame.Speech {
			return nil
		}
	}
	return &domain.NoSpeechEvidence{Backend: analysis.VAD.Backend, Reliable: true}
}

func percentileLinear(sorted []float64, percentile float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := (float64(len(sorted)-1) * percentile) / 100
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	fraction := position - float64(lower)
	return sorted[lower] + (sorted[upper]-sorted[lower])*fraction
}
