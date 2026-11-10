package audio

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	ReasonInsufficientSamples      = "insufficient_samples"
	ReasonContextUnavailable       = "context_unavailable"
	ReasonZeroSpeechDuration       = "zero_speech_duration"
	ReasonTranscriptUnavailable    = "transcript_unavailable"
	ReasonEmptyText                = "empty_text"
	ReasonInsufficientSpeech       = "insufficient_speech"
	ReasonInsufficientVoicedFrames = "insufficient_voiced_frames"

	DefaultMaxMetricUnits = 4096
	maxTextSourceBytes    = 256
	maxUnitTextBytes      = 1 << 20
	minPitchMetricValues  = 3
)

// CandidateMetricRequest binds eligible statements, read-only context, and the
// exact text source to one candidate window. Before/after windows are optional;
// when absent, ContextUnits provide the acoustic baseline. Without any real
// baseline, change-bearing metrics are explicitly unavailable rather than zero.
type CandidateMetricRequest struct {
	Window        domain.AudioInterval
	EligibleUnits []domain.SpeechUnit
	ContextUnits  []domain.SpeechUnit
	Before        *domain.AudioInterval
	After         *domain.AudioInterval
	TextSource    string
}

// CandidateMetrics contains candidate aggregates and exact per-unit timing.
// TextSource applies to every speech-rate measurement in this result.
type CandidateMetrics struct {
	Loudness    domain.LoudnessMetrics    `json:"loudness"`
	SpeechRate  domain.SpeechRateMetrics  `json:"speech_rate"`
	Pitch       domain.PitchMetrics       `json:"pitch"`
	UnitMetrics []domain.UnitAudioMetrics `json:"unit_metrics"`
	TextSource  string                    `json:"text_source,omitempty"`
}

func (m CandidateMetrics) Validate(eligible []domain.SpeechUnit) error {
	if err := m.Loudness.Validate(); err != nil {
		return err
	}
	if err := m.SpeechRate.Validate(); err != nil {
		return err
	}
	if err := m.Pitch.Validate(); err != nil {
		return err
	}
	if err := domain.ValidateUnitAudioMetrics(m.UnitMetrics, eligible); err != nil {
		return err
	}
	if (m.SpeechRate.Available || speechRateUnitsAvailable(m.UnitMetrics)) && strings.TrimSpace(m.TextSource) == "" {
		return fmt.Errorf("%w: available speech rate requires a text source", ErrAnalysis)
	}
	return nil
}

func speechRateUnitsAvailable(metrics []domain.UnitAudioMetrics) bool {
	for _, metric := range metrics {
		if metric.SpeechRate.Available {
			return true
		}
	}
	return false
}

// CalculateCandidateMetrics computes deterministic RMS-mean dBFS, peak dBFS,
// P90 dBFS, bounded-pitch aggregates, effective-character speech rates, and
// before/after changes. All values remain associated with exact statement or
// candidate times; no emotion or speaker label is inferred.
func CalculateCandidateMetrics(frames []PCMFrame, speech []domain.AudioInterval, request CandidateMetricRequest) (CandidateMetrics, error) {
	if err := validateFrames(frames); err != nil {
		return CandidateMetrics{}, err
	}
	if len(speech) > DefaultMaxIntervals {
		return CandidateMetrics{}, fmt.Errorf("%w: too many speech intervals", ErrResourceLimit)
	}
	timelineEnd := frames[len(frames)-1].EndSec
	if err := request.Window.Validate(); err != nil || request.Window.EndSec > timelineEnd+1e-9 {
		return CandidateMetrics{}, fmt.Errorf("%w: invalid candidate metric window", ErrTimeline)
	}
	if err := validateSpeechTimeline(speech, timelineEnd); err != nil {
		return CandidateMetrics{}, err
	}
	if len(request.EligibleUnits) > DefaultMaxMetricUnits || len(request.ContextUnits) > DefaultMaxMetricUnits {
		return CandidateMetrics{}, fmt.Errorf("%w: too many metric statements", ErrResourceLimit)
	}
	if len(request.TextSource) > maxTextSourceBytes {
		return CandidateMetrics{}, fmt.Errorf("%w: text source identity is too long", ErrResourceLimit)
	}
	request.TextSource = strings.TrimSpace(request.TextSource)
	if err := validateOptionalContext(request.Before, request.Window, timelineEnd, true); err != nil {
		return CandidateMetrics{}, err
	}
	if err := validateOptionalContext(request.After, request.Window, timelineEnd, false); err != nil {
		return CandidateMetrics{}, err
	}

	eligibleUnits := append([]domain.SpeechUnit(nil), request.EligibleUnits...)
	sort.SliceStable(eligibleUnits, func(i, j int) bool {
		if eligibleUnits[i].StartSec == eligibleUnits[j].StartSec {
			if eligibleUnits[i].EndSec == eligibleUnits[j].EndSec {
				return eligibleUnits[i].ID < eligibleUnits[j].ID
			}
			return eligibleUnits[i].EndSec < eligibleUnits[j].EndSec
		}
		return eligibleUnits[i].StartSec < eligibleUnits[j].StartSec
	})
	allUnits := make([]domain.SpeechUnit, 0, len(eligibleUnits)+len(request.ContextUnits))
	seenIDs := make(map[string]struct{}, cap(allUnits))
	for _, unit := range eligibleUnits {
		if err := validateMetricUnit(unit, request.Window.StartSec, request.Window.EndSec); err != nil {
			return CandidateMetrics{}, err
		}
		if _, exists := seenIDs[unit.ID]; exists {
			return CandidateMetrics{}, fmt.Errorf("%w: duplicate metric statement ID", ErrInvalidRequest)
		}
		seenIDs[unit.ID] = struct{}{}
		allUnits = append(allUnits, unit)
	}
	for _, unit := range request.ContextUnits {
		if err := validateMetricUnit(unit, 0, timelineEnd); err != nil {
			return CandidateMetrics{}, err
		}
		if unit.EndSec > request.Window.StartSec && unit.StartSec < request.Window.EndSec {
			return CandidateMetrics{}, fmt.Errorf("%w: context statement intersects candidate window", ErrInvalidRequest)
		}
		if _, exists := seenIDs[unit.ID]; exists {
			return CandidateMetrics{}, fmt.Errorf("%w: duplicate metric statement ID", ErrInvalidRequest)
		}
		seenIDs[unit.ID] = struct{}{}
		allUnits = append(allUnits, unit)
	}
	sort.SliceStable(allUnits, func(i, j int) bool {
		if allUnits[i].StartSec == allUnits[j].StartSec {
			if allUnits[i].EndSec == allUnits[j].EndSec {
				return allUnits[i].ID < allUnits[j].ID
			}
			return allUnits[i].EndSec < allUnits[j].EndSec
		}
		return allUnits[i].StartSec < allUnits[j].StartSec
	})
	unitIndex := make(map[string]int, len(allUnits))
	for index, unit := range allUnits {
		if index > 0 && unit.StartSec < allUnits[index-1].EndSec {
			return CandidateMetrics{}, fmt.Errorf("%w: statement metric timelines overlap", ErrTimeline)
		}
		unitIndex[unit.ID] = index
	}

	result := CandidateMetrics{TextSource: request.TextSource, UnitMetrics: make([]domain.UnitAudioMetrics, 0, len(eligibleUnits))}
	unitRates := make(map[string]rateMeasurement, len(allUnits))
	if request.TextSource != "" {
		for _, unit := range allUnits {
			unitRates[unit.ID] = measureSpeechRate(unit, speech)
		}
	}

	for _, unit := range eligibleUnits {
		index := unitIndex[unit.ID]
		contexts, contextRates := neighboringUnitContexts(allUnits, index, unitRates)
		metric := domain.UnitAudioMetrics{
			UnitID: unit.ID, StartSec: unit.StartSec, EndSec: unit.EndSec,
			Loudness: calculateLoudness(frames, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec}, contexts),
			Pitch:    calculatePitch(frames, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec}, contexts),
		}
		metric.SpeechRate = statementSpeechRate(request.TextSource, unitRates[unit.ID], contextRates)
		result.UnitMetrics = append(result.UnitMetrics, metric)
	}

	candidateContexts := explicitOrUnitContexts(request)
	result.Loudness = calculateLoudness(frames, request.Window, candidateContexts)
	result.Pitch = calculatePitch(frames, request.Window, candidateContexts)
	result.SpeechRate = aggregateSpeechRate(request.TextSource, eligibleUnits, request.ContextUnits, unitRates)
	if err := result.Validate(eligibleUnits); err != nil {
		return CandidateMetrics{}, err
	}
	return result, nil
}

func validateOptionalContext(context *domain.AudioInterval, window domain.AudioInterval, timelineEnd float64, before bool) error {
	if context == nil {
		return nil
	}
	if err := context.Validate(); err != nil || context.EndSec > timelineEnd+1e-9 {
		return fmt.Errorf("%w: invalid metric context window", ErrTimeline)
	}
	if before && context.EndSec > window.StartSec+1e-9 || !before && context.StartSec+1e-9 < window.EndSec {
		return fmt.Errorf("%w: metric context must remain outside candidate", ErrInvalidRequest)
	}
	return nil
}

func validateMetricUnit(unit domain.SpeechUnit, start, end float64) error {
	if strings.TrimSpace(unit.ID) == "" || unit.ID != strings.TrimSpace(unit.ID) || !domain.ValidRange(unit.StartSec, unit.EndSec) ||
		unit.StartSec+1e-9 < start || unit.EndSec > end+1e-9 {
		return fmt.Errorf("%w: invalid statement metric identity or timeline", ErrTimeline)
	}
	if len(unit.Text) > maxUnitTextBytes {
		return fmt.Errorf("%w: statement text exceeds metric limit", ErrResourceLimit)
	}
	return nil
}

func validateSpeechTimeline(speech []domain.AudioInterval, timelineEnd float64) error {
	previousEnd := 0.0
	for index, interval := range speech {
		if err := interval.Validate(); err != nil || interval.EndSec > timelineEnd+1e-9 || index > 0 && interval.StartSec < previousEnd {
			return fmt.Errorf("%w: speech intervals must be sorted, disjoint and within the audio timeline", ErrTimeline)
		}
		previousEnd = interval.EndSec
	}
	return nil
}

func neighboringUnitContexts(units []domain.SpeechUnit, index int, rates map[string]rateMeasurement) ([]domain.AudioInterval, []rateMeasurement) {
	contexts := make([]domain.AudioInterval, 0, 2)
	contextRates := make([]rateMeasurement, 0, 2)
	if index > 0 {
		unit := units[index-1]
		contexts = append(contexts, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec})
		if rate, exists := rates[unit.ID]; exists {
			contextRates = append(contextRates, rate)
		}
	}
	if index+1 < len(units) {
		unit := units[index+1]
		contexts = append(contexts, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec})
		if rate, exists := rates[unit.ID]; exists {
			contextRates = append(contextRates, rate)
		}
	}
	return contexts, contextRates
}

func explicitOrUnitContexts(request CandidateMetricRequest) []domain.AudioInterval {
	contexts := make([]domain.AudioInterval, 0, 2+len(request.ContextUnits))
	if request.Before != nil {
		contexts = append(contexts, *request.Before)
	}
	if request.After != nil {
		contexts = append(contexts, *request.After)
	}
	if len(contexts) == 0 {
		for _, unit := range request.ContextUnits {
			contexts = append(contexts, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec})
		}
	}
	return contexts
}

type loudnessMeasurement struct {
	meanDBFS float64
	peakDBFS float64
	p90DBFS  float64
	ok       bool
}

func calculateLoudness(frames []PCMFrame, window domain.AudioInterval, contexts []domain.AudioInterval) domain.LoudnessMetrics {
	measurement := measureLoudness(frames, []domain.AudioInterval{window}, true)
	if !measurement.ok {
		return domain.LoudnessMetrics{Reason: ReasonInsufficientSamples}
	}
	baseline := measureLoudness(frames, contexts, false)
	if !baseline.ok {
		return domain.LoudnessMetrics{Reason: ReasonContextUnavailable}
	}
	change := measurement.meanDBFS - baseline.meanDBFS
	return domain.LoudnessMetrics{
		Available: true, MeanDBFS: floatPointer(measurement.meanDBFS), PeakDBFS: floatPointer(measurement.peakDBFS),
		P90DBFS: floatPointer(measurement.p90DBFS), ChangeDB: floatPointer(change),
	}
}

func measureLoudness(frames []PCMFrame, windows []domain.AudioInterval, distribution bool) loudnessMeasurement {
	windows = mergeTimeWindows(windows)
	weightedRMS, weight, peak := 0.0, 0.0, SilenceDBFS
	levels := make([]float64, 0)
	for _, window := range windows {
		start, end := overlappingFrameBounds(frames, window.StartSec, window.EndSec)
		for index := start; index < end; index++ {
			frame := frames[index]
			overlap := math.Min(window.EndSec, frame.EndSec) - math.Max(window.StartSec, frame.StartSec)
			if overlap <= 0 {
				continue
			}
			weightedRMS += frame.RMS * overlap
			weight += overlap
			if frame.PeakDBFS > peak {
				peak = frame.PeakDBFS
			}
			if distribution {
				levels = append(levels, frame.DBFS)
			}
		}
	}
	if weight <= 0 {
		return loudnessMeasurement{}
	}
	measurement := loudnessMeasurement{meanDBFS: amplitudeDBFS(weightedRMS / weight), peakDBFS: peak, ok: true}
	if distribution {
		sort.Float64s(levels)
		measurement.p90DBFS = percentileLinear(levels, 90)
	}
	return measurement
}

func calculatePitch(frames []PCMFrame, window domain.AudioInterval, contexts []domain.AudioInterval) domain.PitchMetrics {
	values := pitchValues(frames, []domain.AudioInterval{window})
	if len(values) < minPitchMetricValues {
		return domain.PitchMetrics{Reason: ReasonInsufficientVoicedFrames}
	}
	baselineValues := pitchValues(frames, contexts)
	if len(baselineValues) < minPitchMetricValues {
		return domain.PitchMetrics{Reason: ReasonContextUnavailable}
	}
	median := percentileLinear(values, 50)
	p90 := percentileLinear(values, 90)
	change := median - percentileLinear(baselineValues, 50)
	return domain.PitchMetrics{
		Available: true, MedianHz: floatPointer(median), P90Hz: floatPointer(p90), ChangeHz: floatPointer(change),
	}
}

func pitchValues(frames []PCMFrame, windows []domain.AudioInterval) []float64 {
	values := make([]float64, 0)
	for _, window := range mergeTimeWindows(windows) {
		start, end := overlappingFrameBounds(frames, window.StartSec, window.EndSec)
		for index := start; index < end; index++ {
			frame := frames[index]
			if frame.EndSec <= window.StartSec || frame.StartSec >= window.EndSec || !frame.PitchValid ||
				!finite(frame.PitchHz) || frame.PitchHz <= 0 || !finite(frame.PitchConfidence) || frame.PitchConfidence <= 0 {
				continue
			}
			values = append(values, frame.PitchHz)
		}
	}
	sort.Float64s(values)
	return values
}

// EffectiveTextCharacters counts Unicode letters and numbers, excluding
// whitespace and punctuation. One CJK character is one speech-rate unit.
func EffectiveTextCharacters(text string) int {
	count := 0
	for _, value := range text {
		if unicode.IsLetter(value) || unicode.IsNumber(value) {
			count++
		}
	}
	return count
}

type rateMeasurement struct {
	characters int
	duration   float64
	rate       float64
	reason     string
}

func measureSpeechRate(unit domain.SpeechUnit, speech []domain.AudioInterval) rateMeasurement {
	characters := EffectiveTextCharacters(unit.Text)
	if characters == 0 {
		return rateMeasurement{reason: ReasonEmptyText}
	}
	duration := speechDuration(speech, domain.AudioInterval{StartSec: unit.StartSec, EndSec: unit.EndSec})
	if duration <= 0 {
		return rateMeasurement{reason: ReasonZeroSpeechDuration}
	}
	return rateMeasurement{characters: characters, duration: duration, rate: float64(characters) / duration}
}

func statementSpeechRate(source string, measurement rateMeasurement, contexts []rateMeasurement) domain.SpeechRateMetrics {
	if source == "" {
		return domain.SpeechRateMetrics{Reason: ReasonTranscriptUnavailable}
	}
	if measurement.reason != "" {
		return domain.SpeechRateMetrics{Reason: measurement.reason}
	}
	baseline, ok := combineRates(contexts)
	if !ok {
		return domain.SpeechRateMetrics{Reason: ReasonContextUnavailable}
	}
	change := measurement.rate - baseline
	return domain.SpeechRateMetrics{
		Available: true, MeanUnitsPerSec: floatPointer(measurement.rate), MaxUnitsPerSec: floatPointer(measurement.rate),
		ChangeUnitsPerSec: floatPointer(change),
	}
}

func aggregateSpeechRate(source string, eligible, context []domain.SpeechUnit, rates map[string]rateMeasurement) domain.SpeechRateMetrics {
	if source == "" {
		return domain.SpeechRateMetrics{Reason: ReasonTranscriptUnavailable}
	}
	if len(eligible) == 0 {
		return domain.SpeechRateMetrics{Reason: ReasonInsufficientSpeech}
	}
	eligibleRates := make([]rateMeasurement, 0, len(eligible))
	maxRate := 0.0
	for _, unit := range eligible {
		measurement, exists := rates[unit.ID]
		if !exists {
			return domain.SpeechRateMetrics{Reason: ReasonTranscriptUnavailable}
		}
		if measurement.reason != "" {
			return domain.SpeechRateMetrics{Reason: measurement.reason}
		}
		eligibleRates = append(eligibleRates, measurement)
		if measurement.rate > maxRate {
			maxRate = measurement.rate
		}
	}
	mean, ok := combineRates(eligibleRates)
	if !ok {
		return domain.SpeechRateMetrics{Reason: ReasonZeroSpeechDuration}
	}
	contextRates := make([]rateMeasurement, 0, len(context))
	for _, unit := range context {
		measurement, exists := rates[unit.ID]
		if !exists || measurement.reason != "" {
			return domain.SpeechRateMetrics{Reason: ReasonContextUnavailable}
		}
		contextRates = append(contextRates, measurement)
	}
	baseline, ok := combineRates(contextRates)
	if !ok {
		return domain.SpeechRateMetrics{Reason: ReasonContextUnavailable}
	}
	change := mean - baseline
	return domain.SpeechRateMetrics{
		Available: true, MeanUnitsPerSec: floatPointer(mean), MaxUnitsPerSec: floatPointer(maxRate),
		ChangeUnitsPerSec: floatPointer(change),
	}
}

func combineRates(measurements []rateMeasurement) (float64, bool) {
	characters, duration := 0, 0.0
	for _, measurement := range measurements {
		if measurement.reason != "" || measurement.duration <= 0 || !finite(measurement.rate) {
			return 0, false
		}
		characters += measurement.characters
		duration += measurement.duration
	}
	if characters == 0 || duration <= 0 {
		return 0, false
	}
	return float64(characters) / duration, true
}

func speechDuration(speech []domain.AudioInterval, window domain.AudioInterval) float64 {
	if len(speech) == 0 {
		return 0
	}
	start := sort.Search(len(speech), func(index int) bool { return speech[index].EndSec > window.StartSec })
	duration := 0.0
	for index := start; index < len(speech) && speech[index].StartSec < window.EndSec; index++ {
		overlap := math.Min(window.EndSec, speech[index].EndSec) - math.Max(window.StartSec, speech[index].StartSec)
		if overlap > 0 {
			duration += overlap
		}
	}
	return duration
}

func overlappingFrameBounds(frames []PCMFrame, startSec, endSec float64) (int, int) {
	start := sort.Search(len(frames), func(index int) bool { return frames[index].EndSec > startSec })
	end := start + sort.Search(len(frames)-start, func(offset int) bool {
		return frames[start+offset].StartSec >= endSec
	})
	return start, end
}

func mergeTimeWindows(windows []domain.AudioInterval) []domain.AudioInterval {
	valid := make([]domain.AudioInterval, 0, len(windows))
	for _, window := range windows {
		if window.Validate() == nil {
			valid = append(valid, window)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].StartSec == valid[j].StartSec {
			return valid[i].EndSec < valid[j].EndSec
		}
		return valid[i].StartSec < valid[j].StartSec
	})
	merged := make([]domain.AudioInterval, 0, len(valid))
	for _, window := range valid {
		if len(merged) == 0 || window.StartSec > merged[len(merged)-1].EndSec {
			merged = append(merged, window)
			continue
		}
		if window.EndSec > merged[len(merged)-1].EndSec {
			merged[len(merged)-1].EndSec = window.EndSec
		}
	}
	return merged
}

func floatPointer(value float64) *float64 { return &value }
