package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	ErrInvalidAudioFeatures      = errors.New("invalid candidate audio features")
	ErrInvalidAudioFeatureJSON   = errors.New("invalid candidate audio feature JSON")
	ErrAudioFeatureResourceLimit = errors.New("candidate audio feature resource limit exceeded")
	ErrAudioFeaturePrivacy       = errors.New("candidate audio features contain forbidden raw media data")
)

const (
	MaxAudioFeatureJSONBytes = 64 << 10
	MaxAudioFeatureIntervals = 2048
	MaxAudioFeatureUnits     = 1024
	MaxAudioSoundEvents      = 256
	maxAudioFeatureJSONDepth = 64
)

var (
	audioFeatureIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)
	audioFeatureBackendPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	audioMetricReasonPattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

var controlledAudioMetricReasons = map[string]struct{}{
	"analysis_unavailable":       {},
	"context_unavailable":        {},
	"empty_text":                 {},
	"incomplete_unit_metrics":    {},
	"insufficient_samples":       {},
	"insufficient_speech":        {},
	"insufficient_voiced_frames": {},
	"metrics_unavailable":        {},
	"no_eligible_units":          {},
	"no_speech":                  {},
	"transcript_unavailable":     {},
	"vad_unreliable":             {},
	"zero_speech_duration":       {},
}

// AudioInterval is the shared time range used by speech, silence and pauses.
type AudioInterval struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
}

func (i AudioInterval) Validate() error {
	if !ValidRange(i.StartSec, i.EndSec) {
		return fmt.Errorf("%w: audio interval must be finite, non-negative and increasing", ErrInvalidAudioFeatures)
	}
	return nil
}

// OverlapInterval adds detector confidence without changing the common time
// interval semantics. It describes possible overlap, never a speaker identity.
type OverlapInterval struct {
	AudioInterval
	Confidence float64 `json:"confidence"`
}

func (i OverlapInterval) Validate() error {
	if err := i.AudioInterval.Validate(); err != nil {
		return err
	}
	if ValidateScore(i.Confidence) != nil {
		return fmt.Errorf("%w: overlap confidence must be finite and between 0 and 1", ErrInvalidAudioFeatures)
	}
	return nil
}

// VADStatus identifies the VAD backend and whether its timeline is reliable.
// Reliable=false is representable so callers cannot confuse degraded analysis
// with confirmed absence of speech.
type VADStatus struct {
	Backend  string `json:"backend"`
	Reliable bool   `json:"reliable"`
}

func (v VADStatus) Validate() error {
	if !audioFeatureBackendPattern.MatchString(v.Backend) {
		return fmt.Errorf("%w: VAD backend must be a bounded safe identifier", ErrInvalidAudioFeatures)
	}
	return nil
}

// LoudnessMetrics contains only finite measurements when Available is true.
// Pointer fields distinguish an explicit zero measurement from a missing value.
type LoudnessMetrics struct {
	Available bool     `json:"available"`
	MeanDBFS  *float64 `json:"mean_dbfs,omitempty"`
	PeakDBFS  *float64 `json:"peak_dbfs,omitempty"`
	P90DBFS   *float64 `json:"p90_dbfs,omitempty"`
	ChangeDB  *float64 `json:"change_db,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

func (m LoudnessMetrics) Validate() error {
	return validateMetricAvailability(m.Available, m.Reason, m.MeanDBFS, m.PeakDBFS, m.P90DBFS, m.ChangeDB)
}

type SpeechRateMetrics struct {
	Available         bool     `json:"available"`
	MeanUnitsPerSec   *float64 `json:"mean_units_per_sec,omitempty"`
	MaxUnitsPerSec    *float64 `json:"max_units_per_sec,omitempty"`
	ChangeUnitsPerSec *float64 `json:"change_units_per_sec,omitempty"`
	Reason            string   `json:"reason,omitempty"`
}

func (m SpeechRateMetrics) Validate() error {
	return validateMetricAvailability(m.Available, m.Reason, m.MeanUnitsPerSec, m.MaxUnitsPerSec, m.ChangeUnitsPerSec)
}

type PitchMetrics struct {
	Available bool     `json:"available"`
	MedianHz  *float64 `json:"median_hz,omitempty"`
	P90Hz     *float64 `json:"p90_hz,omitempty"`
	ChangeHz  *float64 `json:"change_hz,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

func (m PitchMetrics) Validate() error {
	return validateMetricAvailability(m.Available, m.Reason, m.MedianHz, m.P90Hz, m.ChangeHz)
}

func validateMetricAvailability(available bool, reason string, values ...*float64) error {
	if available {
		if reason != "" {
			return fmt.Errorf("%w: available metric cannot have an unavailable reason", ErrInvalidAudioFeatures)
		}
		for _, value := range values {
			if value == nil || !Finite(*value) {
				return fmt.Errorf("%w: available metric requires complete finite values", ErrInvalidAudioFeatures)
			}
		}
		return nil
	}
	if !audioMetricReasonPattern.MatchString(reason) {
		return fmt.Errorf("%w: unavailable metric requires a trimmed reason code", ErrInvalidAudioFeatures)
	}
	if _, controlled := controlledAudioMetricReasons[reason]; !controlled {
		return fmt.Errorf("%w: unavailable metric reason is not controlled", ErrInvalidAudioFeatures)
	}
	for _, value := range values {
		if value != nil {
			return fmt.Errorf("%w: unavailable metric cannot carry measured values", ErrInvalidAudioFeatures)
		}
	}
	return nil
}

// UnitAudioMetrics associates measurements with one eligible SpeechUnit.
type UnitAudioMetrics struct {
	UnitID     string            `json:"unit_id"`
	StartSec   float64           `json:"start_sec"`
	EndSec     float64           `json:"end_sec"`
	Loudness   LoudnessMetrics   `json:"loudness"`
	SpeechRate SpeechRateMetrics `json:"speech_rate"`
	Pitch      PitchMetrics      `json:"pitch"`
}

func (m UnitAudioMetrics) Validate() error {
	if !audioFeatureIdentifierPattern.MatchString(m.UnitID) {
		return fmt.Errorf("%w: unit metric ID is required and must be safe", ErrInvalidAudioFeatures)
	}
	if !ValidRange(m.StartSec, m.EndSec) {
		return fmt.Errorf("%w: unit metric time range is invalid", ErrInvalidAudioFeatures)
	}
	if err := m.Loudness.Validate(); err != nil {
		return fmt.Errorf("unit loudness: %w", err)
	}
	if err := m.SpeechRate.Validate(); err != nil {
		return fmt.Errorf("unit speech rate: %w", err)
	}
	if err := m.Pitch.Validate(); err != nil {
		return fmt.Errorf("unit pitch: %w", err)
	}
	return nil
}

// ValidateUnitAudioMetrics verifies unit ownership, uniqueness and exact time
// association. Context units are deliberately not eligible metric owners.
func ValidateUnitAudioMetrics(metrics []UnitAudioMetrics, eligibleUnits []SpeechUnit) error {
	eligible := make(map[string]SpeechUnit, len(eligibleUnits))
	for _, unit := range eligibleUnits {
		if !audioFeatureIdentifierPattern.MatchString(unit.ID) {
			return fmt.Errorf("%w: eligible unit ID is required and must be safe", ErrInvalidAudioFeatures)
		}
		if _, exists := eligible[unit.ID]; exists {
			return fmt.Errorf("%w: duplicate eligible unit ID", ErrInvalidAudioFeatures)
		}
		eligible[unit.ID] = unit
	}
	seen := make(map[string]struct{}, len(metrics))
	for _, metric := range metrics {
		if err := metric.Validate(); err != nil {
			return err
		}
		if _, exists := seen[metric.UnitID]; exists {
			return fmt.Errorf("%w: duplicate unit metric ID", ErrInvalidAudioFeatures)
		}
		seen[metric.UnitID] = struct{}{}
		unit, exists := eligible[metric.UnitID]
		if !exists {
			return fmt.Errorf("%w: unit metric references an unknown or ineligible unit", ErrInvalidAudioFeatures)
		}
		if metric.StartSec != unit.StartSec || metric.EndSec != unit.EndSec {
			return fmt.Errorf("%w: unit metric time range does not match its speech unit", ErrInvalidAudioFeatures)
		}
	}
	return nil
}

type SoundEventType string

const (
	SoundEventTypeLaughter    SoundEventType = "laughter"
	SoundEventTypeApplause    SoundEventType = "applause"
	SoundEventTypeMusicClimax SoundEventType = "music_climax"
)

var ErrInvalidSoundEventType = errors.New("invalid highlight sound event type")

func ParseSoundEventType(value string) (SoundEventType, error) {
	eventType := SoundEventType(value)
	switch eventType {
	case SoundEventTypeLaughter, SoundEventTypeApplause, SoundEventTypeMusicClimax:
		return eventType, nil
	default:
		return "", ErrInvalidSoundEventType
	}
}

type SoundEvent struct {
	Type       SoundEventType `json:"type"`
	StartSec   float64        `json:"start_sec"`
	EndSec     float64        `json:"end_sec"`
	Confidence float64        `json:"confidence"`
}

func (e SoundEvent) Validate() error {
	if _, err := ParseSoundEventType(string(e.Type)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAudioFeatures, err)
	}
	if !ValidRange(e.StartSec, e.EndSec) {
		return fmt.Errorf("%w: sound event time range is invalid", ErrInvalidAudioFeatures)
	}
	if ValidateScore(e.Confidence) != nil {
		return fmt.Errorf("%w: sound event confidence must be finite and between 0 and 1", ErrInvalidAudioFeatures)
	}
	return nil
}

// CandidateAudioFeatures is the audio_features_v1 wire model. Timeline
// ordering, containment, resource limits and strict wire decoding are enforced
// by the shared wire validator at the scoring/API boundary.
type CandidateAudioFeatures struct {
	Schema      string             `json:"schema"`
	Window      AudioInterval      `json:"window"`
	VAD         VADStatus          `json:"vad"`
	Speech      []AudioInterval    `json:"speech"`
	Silence     []AudioInterval    `json:"silence"`
	Pauses      []AudioInterval    `json:"pauses"`
	Overlaps    []OverlapInterval  `json:"overlaps"`
	Loudness    LoudnessMetrics    `json:"loudness"`
	SpeechRate  SpeechRateMetrics  `json:"speech_rate"`
	Pitch       PitchMetrics       `json:"pitch"`
	UnitMetrics []UnitAudioMetrics `json:"unit_metrics"`
	SoundEvents []SoundEvent       `json:"sound_events"`
}

// Validate checks the model invariants introduced with the wire types. It does
// not replace the complete shared timeline/resource validator.
func (f CandidateAudioFeatures) Validate(eligibleUnits []SpeechUnit) error {
	if f.Schema != AudioFeatureSchemaV1 {
		return fmt.Errorf("%w: unsupported audio feature schema", ErrInvalidAudioFeatures)
	}
	if err := f.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if err := f.VAD.Validate(); err != nil {
		return err
	}
	for _, intervals := range [][]AudioInterval{f.Speech, f.Silence, f.Pauses} {
		for _, interval := range intervals {
			if err := interval.Validate(); err != nil {
				return err
			}
		}
	}
	for _, overlap := range f.Overlaps {
		if err := overlap.Validate(); err != nil {
			return err
		}
	}
	if err := f.Loudness.Validate(); err != nil {
		return fmt.Errorf("candidate loudness: %w", err)
	}
	if err := f.SpeechRate.Validate(); err != nil {
		return fmt.Errorf("candidate speech rate: %w", err)
	}
	if err := f.Pitch.Validate(); err != nil {
		return fmt.Errorf("candidate pitch: %w", err)
	}
	if err := ValidateUnitAudioMetrics(f.UnitMetrics, eligibleUnits); err != nil {
		return err
	}
	for _, event := range f.SoundEvents {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// AudioFeatureValidationWindow binds a feature payload to the exact candidate
// context window and media timeline on both local and server boundaries.
type AudioFeatureValidationWindow struct {
	StartSec         float64
	EndSec           float64
	MediaDurationSec float64
	EligibleUnits    []SpeechUnit
	ContextUnits     []SpeechUnit
}

// ValidateAudioFeatures is the complete shared audio_features_v1 validator.
// CandidateAudioFeatures.Validate intentionally remains the smaller model-level
// check used by older callers; wire callers must use this function.
func ValidateAudioFeatures(features CandidateAudioFeatures, window AudioFeatureValidationWindow) error {
	if !ValidRange(window.StartSec, window.EndSec) || !Finite(window.MediaDurationSec) ||
		window.MediaDurationSec <= 0 || window.EndSec > window.MediaDurationSec {
		return fmt.Errorf("%w: audio feature validation window is invalid", ErrInvalidAudioFeatures)
	}
	if features.Window.StartSec != window.StartSec || features.Window.EndSec != window.EndSec {
		return fmt.Errorf("%w: audio feature window does not match its validation window", ErrInvalidAudioFeatures)
	}
	if len(window.EligibleUnits) > MaxAudioFeatureUnits || len(window.ContextUnits) > MaxAudioFeatureUnits {
		return fmt.Errorf("%w: %w: too many speech units", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	if features.Speech == nil || features.Silence == nil || features.Pauses == nil ||
		features.Overlaps == nil || features.UnitMetrics == nil || features.SoundEvents == nil {
		return fmt.Errorf("%w: audio feature arrays must be present", ErrInvalidAudioFeatures)
	}
	if len(features.Speech) > MaxAudioFeatureIntervals || len(features.Silence) > MaxAudioFeatureIntervals ||
		len(features.Pauses) > MaxAudioFeatureIntervals || len(features.Overlaps) > MaxAudioFeatureIntervals ||
		len(features.UnitMetrics) > MaxAudioFeatureUnits || len(features.SoundEvents) > MaxAudioSoundEvents {
		return fmt.Errorf("%w: %w: audio feature array limit", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	if err := features.Validate(window.EligibleUnits); err != nil {
		return err
	}
	if err := validateAudioFeatureUnits(window.EligibleUnits, features.Window); err != nil {
		return err
	}
	if err := validateAudioFeatureUnits(window.ContextUnits, features.Window); err != nil {
		return err
	}
	for _, intervals := range [][]AudioInterval{features.Speech, features.Silence, features.Pauses} {
		if err := validateAudioFeatureIntervals(intervals, features.Window, true); err != nil {
			return err
		}
	}
	overlaps := make([]AudioInterval, len(features.Overlaps))
	for index, overlap := range features.Overlaps {
		overlaps[index] = overlap.AudioInterval
		if index > 0 {
			previous := features.Overlaps[index-1]
			if audioIntervalLess(overlap.AudioInterval, previous.AudioInterval) {
				return fmt.Errorf("%w: overlap intervals are not sorted", ErrInvalidAudioFeatures)
			}
			if previous.StartSec == overlap.StartSec && previous.EndSec == overlap.EndSec {
				return fmt.Errorf("%w: duplicate overlap interval", ErrInvalidAudioFeatures)
			}
		}
	}
	if err := validateAudioFeatureIntervals(overlaps, features.Window, false); err != nil {
		return err
	}
	for index, event := range features.SoundEvents {
		if event.StartSec < features.Window.StartSec || event.EndSec > features.Window.EndSec {
			return fmt.Errorf("%w: sound event is outside the feature window", ErrInvalidAudioFeatures)
		}
		if index > 0 {
			previous := features.SoundEvents[index-1]
			if event.StartSec < previous.StartSec || event.StartSec == previous.StartSec && event.EndSec < previous.EndSec {
				return fmt.Errorf("%w: sound events are not sorted", ErrInvalidAudioFeatures)
			}
		}
	}
	for index, metric := range features.UnitMetrics {
		if index == 0 {
			continue
		}
		previous := features.UnitMetrics[index-1]
		if metric.StartSec < previous.StartSec ||
			metric.StartSec == previous.StartSec && (metric.EndSec < previous.EndSec ||
				metric.EndSec == previous.EndSec && metric.UnitID < previous.UnitID) {
			return fmt.Errorf("%w: unit metrics are not sorted", ErrInvalidAudioFeatures)
		}
	}
	encoded, err := json.Marshal(features)
	if err != nil {
		return fmt.Errorf("%w: audio features cannot be encoded safely", ErrInvalidAudioFeatures)
	}
	if len(encoded) > MaxAudioFeatureJSONBytes {
		return fmt.Errorf("%w: %w: audio feature JSON exceeds 64 KiB", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	return nil
}

// ValidateCandidateAudioFeatures is an explicit alias for callers that prefer
// the wire type name over the shorter shared protocol name.
func ValidateCandidateAudioFeatures(features CandidateAudioFeatures, window AudioFeatureValidationWindow) error {
	return ValidateAudioFeatures(features, window)
}

func validateAudioFeatureUnits(units []SpeechUnit, window AudioInterval) error {
	previousStart := -1.0
	for _, unit := range units {
		if !audioFeatureIdentifierPattern.MatchString(unit.ID) || !ValidRange(unit.StartSec, unit.EndSec) ||
			unit.StartSec < window.StartSec || unit.EndSec > window.EndSec || unit.StartSec < previousStart {
			return fmt.Errorf("%w: speech unit is unsafe or outside the feature window", ErrInvalidAudioFeatures)
		}
		previousStart = unit.StartSec
	}
	return nil
}

func validateAudioFeatureIntervals(intervals []AudioInterval, window AudioInterval, rejectOverlap bool) error {
	for index, interval := range intervals {
		if interval.StartSec < window.StartSec || interval.EndSec > window.EndSec {
			return fmt.Errorf("%w: audio interval is outside the feature window", ErrInvalidAudioFeatures)
		}
		if index == 0 {
			continue
		}
		previous := intervals[index-1]
		if audioIntervalLess(interval, previous) {
			return fmt.Errorf("%w: audio intervals are not sorted", ErrInvalidAudioFeatures)
		}
		if rejectOverlap && interval.StartSec < previous.EndSec {
			return fmt.Errorf("%w: audio intervals of the same kind overlap", ErrInvalidAudioFeatures)
		}
	}
	return nil
}

func audioIntervalLess(left, right AudioInterval) bool {
	return left.StartSec < right.StartSec || left.StartSec == right.StartSec && left.EndSec < right.EndSec
}

// DecodeAudioFeaturesJSON strictly decodes and validates exactly one feature
// object. It rejects unknown or case-variant fields, recursive duplicates,
// trailing JSON, oversized/deep payloads and raw-media smuggling before any
// authorization or model call.
func DecodeAudioFeaturesJSON(data []byte, window AudioFeatureValidationWindow) (CandidateAudioFeatures, error) {
	if len(data) == 0 {
		return CandidateAudioFeatures{}, invalidAudioFeatureJSON("empty audio feature JSON")
	}
	if len(data) > MaxAudioFeatureJSONBytes {
		return CandidateAudioFeatures{}, fmt.Errorf("%w: %w: audio feature JSON exceeds 64 KiB", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	if err := scanAudioFeatureJSON(data); err != nil {
		return CandidateAudioFeatures{}, err
	}
	if err := validateExactAudioFeatureFields(data, reflect.TypeOf(CandidateAudioFeatures{}), 0); err != nil {
		return CandidateAudioFeatures{}, invalidAudioFeatureJSON("unknown or incorrectly cased audio feature field")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var features CandidateAudioFeatures
	if err := decoder.Decode(&features); err != nil {
		return CandidateAudioFeatures{}, invalidAudioFeatureJSON("audio feature JSON does not match audio_features_v1")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return CandidateAudioFeatures{}, invalidAudioFeatureJSON("trailing JSON is not allowed")
	}
	if err := ValidateAudioFeatures(features, window); err != nil {
		return CandidateAudioFeatures{}, err
	}
	return features, nil
}

// DecodeCandidateAudioFeaturesJSON is the wire-type-named alias used by new
// adapters; both names execute exactly the same parser and validator.
func DecodeCandidateAudioFeaturesJSON(data []byte, window AudioFeatureValidationWindow) (CandidateAudioFeatures, error) {
	return DecodeAudioFeaturesJSON(data, window)
}

func invalidAudioFeatureJSON(message string) error {
	return fmt.Errorf("%w: %w: %s", ErrInvalidAudioFeatures, ErrInvalidAudioFeatureJSON, message)
}

func scanAudioFeatureJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanAudioFeatureJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalidAudioFeatureJSON("trailing JSON is not allowed")
	}
	return nil
}

func scanAudioFeatureJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxAudioFeatureJSONDepth {
		return fmt.Errorf("%w: %w: audio feature JSON is too deep", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	token, err := decoder.Token()
	if err != nil {
		return invalidAudioFeatureJSON("malformed audio feature JSON")
	}
	if text, ok := token.(string); ok && looksLikeRawAudioPayload(text) {
		return fmt.Errorf("%w: %w", ErrInvalidAudioFeatures, ErrAudioFeaturePrivacy)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return invalidAudioFeatureJSON("malformed audio feature object")
			}
			key, ok := keyToken.(string)
			if !ok {
				return invalidAudioFeatureJSON("audio feature object key is not a string")
			}
			folded := strings.ToLower(key)
			if _, duplicate := seen[folded]; duplicate {
				return invalidAudioFeatureJSON("duplicate or case-variant audio feature field")
			}
			seen[folded] = struct{}{}
			if forbiddenAudioFeatureJSONKey(key) {
				return fmt.Errorf("%w: %w", ErrInvalidAudioFeatures, ErrAudioFeaturePrivacy)
			}
			if err := scanAudioFeatureJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return invalidAudioFeatureJSON("unterminated audio feature object")
		}
	case '[':
		for decoder.More() {
			if err := scanAudioFeatureJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return invalidAudioFeatureJSON("unterminated audio feature array")
		}
	default:
		return invalidAudioFeatureJSON("unexpected audio feature JSON delimiter")
	}
	return nil
}

func forbiddenAudioFeatureJSONKey(key string) bool {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	switch normalized {
	case "pcm", "pcmdata", "pcmsamples", "samples", "samplearray", "waveform",
		"spectrum", "spectralframes", "spectrogram", "fft", "fftframes",
		"wav", "wavpath", "audiopath", "rawaudio", "audiobytes", "audiobase64",
		"database64", "base64", "base64data", "encodedaudio", "mediafile", "mediapath":
		return true
	default:
		return false
	}
}

func looksLikeRawAudioPayload(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(lower, ".wav") || strings.Contains(lower, ".pcm") ||
		strings.Contains(lower, "data:audio/") || strings.Contains(lower, ";base64,") {
		return true
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	if len(compact) < 64 {
		return false
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err == nil && len(decoded) >= 32 && encoding.EncodeToString(decoded) == compact {
			return true
		}
	}
	return false
}

func validateExactAudioFeatureFields(data []byte, valueType reflect.Type, depth int) error {
	if depth > maxAudioFeatureJSONDepth {
		return fmt.Errorf("%w: %w: audio feature JSON is too deep", ErrInvalidAudioFeatures, ErrAudioFeatureResourceLimit)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	switch valueType.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		fields := audioFeatureJSONFields(valueType)
		for name, raw := range object {
			fieldType, exists := fields[name]
			if !exists {
				return errors.New("unknown audio feature field")
			}
			if err := validateExactAudioFeatureFields(raw, fieldType, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var elements []json.RawMessage
		if err := json.Unmarshal(data, &elements); err != nil {
			return err
		}
		for _, element := range elements {
			if err := validateExactAudioFeatureFields(element, valueType.Elem(), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func audioFeatureJSONFields(structType reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if field.Anonymous && name == "" && fieldType.Kind() == reflect.Struct {
			for embeddedName, embeddedType := range audioFeatureJSONFields(fieldType) {
				fields[embeddedName] = embeddedType
			}
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

// SortedSoundEvents returns a detached, deterministically ordered event slice.
// Projection uses it before applying the non-safety event cap.
func SortedSoundEvents(events []SoundEvent) []SoundEvent {
	ordered := append([]SoundEvent{}, events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].StartSec != ordered[j].StartSec {
			return ordered[i].StartSec < ordered[j].StartSec
		}
		if ordered[i].EndSec != ordered[j].EndSec {
			return ordered[i].EndSec < ordered[j].EndSec
		}
		return ordered[i].Type < ordered[j].Type
	})
	return ordered
}
