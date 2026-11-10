package audio

import (
	"context"
	"fmt"
	"math"
)

const (
	pitchWindowDurationSec = 0.040
	absoluteMinPitchHz     = 50.0
	absoluteMaxPitchHz     = 500.0
	spectralGridHz         = 5.0
)

// PitchOptions bounds frequency, voicing confidence, and work. Zero values use
// deterministic defaults. Autocorrelation uses a fixed 40 ms observation window
// centered on each 20 ms metric frame; frame/hop semantics remain unchanged.
type PitchOptions struct {
	MinHz               float64
	MaxHz               float64
	MinRMS              float64
	MinConfidence       float64
	MaxZeroCrossingRate float64
	MaxFrames           int
}

func defaultPitchOptions() PitchOptions {
	return PitchOptions{
		MinHz:               70,
		MaxHz:               350,
		MinRMS:              0.003,
		MinConfidence:       0.55,
		MaxZeroCrossingRate: 0.30,
		MaxFrames:           DefaultMaxFrames,
	}
}

func (o PitchOptions) normalized() (PitchOptions, error) {
	defaults := defaultPitchOptions()
	if o.MinHz == 0 {
		o.MinHz = defaults.MinHz
	}
	if o.MaxHz == 0 {
		o.MaxHz = defaults.MaxHz
	}
	if o.MinRMS == 0 {
		o.MinRMS = defaults.MinRMS
	}
	if o.MinConfidence == 0 {
		o.MinConfidence = defaults.MinConfidence
	}
	if o.MaxZeroCrossingRate == 0 {
		o.MaxZeroCrossingRate = defaults.MaxZeroCrossingRate
	}
	if o.MaxFrames == 0 {
		o.MaxFrames = defaults.MaxFrames
	}
	return o.validated()
}

func (o PitchOptions) validated() (PitchOptions, error) {
	values := []float64{o.MinHz, o.MaxHz, o.MinRMS, o.MinConfidence, o.MaxZeroCrossingRate}
	for _, value := range values {
		if !finite(value) {
			return PitchOptions{}, fmt.Errorf("%w: pitch options must be finite", ErrInvalidRequest)
		}
	}
	if o.MinHz < absoluteMinPitchHz || o.MaxHz > absoluteMaxPitchHz || o.MinHz >= o.MaxHz ||
		o.MinRMS <= 0 || o.MinRMS > 1 || o.MinConfidence <= 0 || o.MinConfidence > 1 ||
		o.MaxZeroCrossingRate <= 0 || o.MaxZeroCrossingRate > 1 || o.MaxFrames < 1 || o.MaxFrames > DefaultMaxFrames {
		return PitchOptions{}, fmt.Errorf("%w: invalid bounded pitch options", ErrInvalidRequest)
	}
	return o, nil
}

type pitchPeak struct {
	hz         float64
	confidence float64
	lag        int
}

type pitchObservation struct {
	primary       pitchPeak
	first         pitchPeak
	second        pitchPeak
	hasMultiPitch bool
}

type spectralBasis struct {
	count       int
	frequencies []float64
	cosine      []float64
	sine        []float64
}

func newSpectralBasis(minHz, maxHz float64, sampleRate, count int) *spectralBasis {
	first := int(math.Ceil(minHz / spectralGridHz))
	last := int(math.Floor(maxHz / spectralGridHz))
	if first < 1 || last < first || count < 1 {
		return nil
	}
	frequencies := make([]float64, 0, last-first+1)
	for index := first; index <= last; index++ {
		frequencies = append(frequencies, float64(index)*spectralGridHz)
	}
	basis := &spectralBasis{
		count: count, frequencies: frequencies,
		cosine: make([]float64, len(frequencies)*count), sine: make([]float64, len(frequencies)*count),
	}
	for frequencyIndex, frequency := range frequencies {
		for sample := 0; sample < count; sample++ {
			phase := 2 * math.Pi * frequency * float64(sample) / float64(sampleRate)
			offset := frequencyIndex*count + sample
			basis.cosine[offset] = math.Cos(phase)
			basis.sine[offset] = math.Sin(phase)
		}
	}
	return basis
}

// EstimatePitch returns a copy of frames with explicit pitch validity. Invalid,
// silent, noisy, short, or low-confidence frames never expose a substitute 0 Hz
// measurement through PitchValid.
func EstimatePitch(ctx context.Context, pcm PCM, frames []PCMFrame, options ...PitchOptions) ([]PCMFrame, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("%w: multiple pitch options", ErrInvalidRequest)
	}
	option := PitchOptions{}
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
	pcmDuration := float64(len(pcm.Samples)) / float64(pcm.Format.SampleRate)
	if pcm.DurationSec != 0 && (!positiveFinite(pcm.DurationSec) || math.Abs(pcm.DurationSec-pcmDuration) > 1/float64(pcm.Format.SampleRate)) {
		return nil, fmt.Errorf("%w: pitch PCM duration does not match its sample count", ErrTimeline)
	}
	if err := validateFrames(frames); err != nil {
		return nil, err
	}
	if len(frames) > option.MaxFrames {
		return nil, fmt.Errorf("%w: pitch frames exceed %d", ErrResourceLimit, option.MaxFrames)
	}
	sampleRate := pcm.Format.SampleRate
	for _, frame := range frames {
		if frame.StartSample < 0 || frame.EndSample <= frame.StartSample || frame.EndSample > len(pcm.Samples) ||
			math.Abs(frame.StartSec-float64(frame.StartSample)/float64(sampleRate)) > 1e-9 ||
			math.Abs(frame.EndSec-float64(frame.EndSample)/float64(sampleRate)) > 1e-9 {
			return nil, fmt.Errorf("%w: pitch frame sample bounds or timeline are invalid", ErrTimeline)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	minLag := int(math.Floor(float64(sampleRate) / option.MaxHz))
	maxLag := int(math.Ceil(float64(sampleRate) / option.MinHz))
	windowSamples := int(math.Round(float64(sampleRate) * pitchWindowDurationSec))
	if minLag < 2 || maxLag <= minLag || maxLag*2 > windowSamples {
		return nil, fmt.Errorf("%w: pitch frequency bounds do not fit the observation window", ErrInvalidRequest)
	}
	result := append([]PCMFrame(nil), frames...)
	signal := make([]float64, windowSamples)
	prefixEnergy := make([]float64, windowSamples+1)
	correlation := make([]float64, maxLag-minLag+1)
	basis := newSpectralBasis(option.MinHz, option.MaxHz, sampleRate, windowSamples)
	spectralPower := make([]float64, 0)
	if basis != nil {
		spectralPower = make([]float64, len(basis.frequencies))
	}
	for index := range result {
		if index%frameCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		frame := &result[index]
		frame.PitchValid = false
		frame.PitchHz = 0
		frame.PitchConfidence = 0
		frame.pitchAnalyzed = true
		frame.pitch = pitchObservation{}
		if frame.RMS < option.MinRMS || frame.ZeroCrossingRate > option.MaxZeroCrossingRate {
			continue
		}
		start, end := centeredPitchWindow(*frame, len(pcm.Samples), windowSamples)
		observation := analyzePitchWindow(pcm.Samples[start:end], minLag, maxLag, option, signal[:end-start], prefixEnergy[:end-start+1], correlation, basis, spectralPower)
		frame.pitch = observation
		if observation.primary.confidence >= option.MinConfidence && finite(observation.primary.hz) &&
			observation.primary.hz >= option.MinHz && observation.primary.hz <= option.MaxHz {
			frame.PitchValid = true
			frame.PitchHz = observation.primary.hz
			frame.PitchConfidence = observation.primary.confidence
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func centeredPitchWindow(frame PCMFrame, totalSamples, windowSamples int) (int, int) {
	if totalSamples <= windowSamples {
		return 0, totalSamples
	}
	center := (frame.StartSample + frame.EndSample) / 2
	start := center - windowSamples/2
	if start < 0 {
		start = 0
	}
	if start+windowSamples > totalSamples {
		start = totalSamples - windowSamples
	}
	return start, start + windowSamples
}

func analyzePitchWindow(samples []int16, minLag, maxLag int, option PitchOptions, signal, prefixEnergy, correlation []float64, basis *spectralBasis, spectralPower []float64) pitchObservation {
	count := len(samples)
	if count <= maxLag || count < minLag*2+1 || count > len(signal) {
		return pitchObservation{}
	}
	mean := 0.0
	for _, sample := range samples {
		mean += float64(sample) / 32768.0
	}
	mean /= float64(count)
	prefixEnergy[0] = 0
	for index, sample := range samples {
		value := float64(sample)/32768.0 - mean
		signal[index] = value
		prefixEnergy[index+1] = prefixEnergy[index] + value*value
	}
	if prefixEnergy[count] <= 1e-12 {
		return pitchObservation{}
	}

	bestLag := -1
	bestConfidence := -1.0
	for lag := minLag; lag <= maxLag; lag++ {
		sum := 0.0
		for index := 0; index+lag < count; index++ {
			sum += signal[index] * signal[index+lag]
		}
		leftEnergy := prefixEnergy[count-lag]
		rightEnergy := prefixEnergy[count] - prefixEnergy[lag]
		normalizer := math.Sqrt(leftEnergy * rightEnergy)
		value := 0.0
		if normalizer > 1e-12 {
			value = sum / normalizer
		}
		correlation[lag-minLag] = value
		if value > bestConfidence {
			bestConfidence = value
			bestLag = lag
		}
	}
	if bestLag < 0 {
		return pitchObservation{}
	}

	// The largest correlation can be a multiple of the true period (for example
	// 240 Hz misreported as 80 Hz). Select the earliest strong local period peak.
	primaryLag := bestLag
	primaryThreshold := math.Max(option.MinConfidence, bestConfidence*0.90)
	for index := 1; index < len(correlation)-1; index++ {
		value := correlation[index]
		if value >= primaryThreshold && value >= correlation[index-1] && value >= correlation[index+1] {
			primaryLag = minLag + index
			break
		}
	}
	interpolatedLag := float64(primaryLag)
	primaryIndex := primaryLag - minLag
	primaryConfidence := correlation[primaryIndex]
	if primaryIndex > 0 && primaryIndex < len(correlation)-1 {
		left := correlation[primaryIndex-1]
		center := correlation[primaryIndex]
		right := correlation[primaryIndex+1]
		denominator := left - 2*center + right
		if denominator < -1e-12 {
			delta := 0.5 * (left - right) / denominator
			if math.Abs(delta) <= 1 {
				interpolatedLag += delta
			}
		}
	}
	observation := pitchObservation{primary: pitchPeak{
		hz: float64(AnalysisSampleRate) / interpolatedLag, confidence: clampUnit(primaryConfidence), lag: primaryLag,
	}}
	if basis != nil && basis.count == count && len(spectralPower) == len(basis.frequencies) {
		observation.first, observation.second, observation.hasMultiPitch = detectSpectralPair(signal[:count], basis, prefixEnergy[count], spectralPower)
	}
	return observation
}

type spectralPeak struct {
	pitchPeak
	power float64
}

// detectSpectralPair uses a fixed, precomputed 5 Hz grid. Two peaks must both
// carry absolute energy, be comparable in strength, and be non-harmonic. This is
// intentionally only an overlap clue; it does not identify speakers.
func detectSpectralPair(signal []float64, basis *spectralBasis, energy float64, power []float64) (pitchPeak, pitchPeak, bool) {
	count := len(signal)
	if count == 0 || energy <= 1e-12 {
		return pitchPeak{}, pitchPeak{}, false
	}
	for frequencyIndex := range basis.frequencies {
		realPart, imaginaryPart := 0.0, 0.0
		offset := frequencyIndex * count
		for sample, value := range signal {
			realPart += value * basis.cosine[offset+sample]
			imaginaryPart -= value * basis.sine[offset+sample]
		}
		power[frequencyIndex] = (realPart*realPart + imaginaryPart*imaginaryPart) / (energy * float64(count))
	}
	peaks := make([]spectralPeak, 0, 8)
	for index := 1; index < len(power)-1; index++ {
		value := power[index]
		if value < 0.025 || value < power[index-1] || value < power[index+1] {
			continue
		}
		confidence := 0.85 * math.Min(1, value/0.08)
		peaks = append(peaks, spectralPeak{
			pitchPeak: pitchPeak{hz: basis.frequencies[index], confidence: clampUnit(confidence)}, power: value,
		})
	}
	bestScore := 0.0
	var first, second pitchPeak
	found := false
	for left := 0; left < len(peaks); left++ {
		for right := left + 1; right < len(peaks); right++ {
			a, b := peaks[left], peaks[right]
			if harmonicPitchPair(a.hz, b.hz) || math.Min(a.power, b.power) < 0.20*math.Max(a.power, b.power) {
				continue
			}
			score := math.Min(a.confidence, b.confidence)
			if score > bestScore {
				bestScore = score
				first, second, found = a.pitchPeak, b.pitchPeak, true
			}
		}
	}
	return first, second, found
}

func harmonicPitchPair(first, second float64) bool {
	if first <= 0 || second <= 0 {
		return true
	}
	if math.Abs(first-second)/math.Min(first, second) < 0.08 {
		return true
	}
	ratio := math.Max(first, second) / math.Min(first, second)
	nearest := math.Round(ratio)
	return nearest >= 2 && math.Abs(ratio-nearest) <= 0.08*nearest
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
