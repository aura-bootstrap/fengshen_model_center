package runtimejobs

// These path-free wire DTOs mirror generation_server/highlight/{domain,scoring,
// transcript}. This module currently has no generation_server dependency. No
// image/base64, ContactSheetPath, URL or local-path field crosses this boundary.
// Adapters must explicitly project fields; never marshal a local-media object.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

type TranscriptWord struct {
	StartSec    float64  `json:"start_sec"`
	EndSec      float64  `json:"end_sec"`
	Text        string   `json:"text"`
	Probability *float64 `json:"probability,omitempty"`
}
type TranscriptItem struct {
	StartSec    float64          `json:"start_sec"`
	EndSec      float64          `json:"end_sec"`
	Text        string           `json:"text"`
	Probability *float64         `json:"probability,omitempty"`
	Words       []TranscriptWord `json:"words"`
}
type SpeechUnit struct {
	ID                string           `json:"id"`
	StartSec          float64          `json:"start_sec"`
	EndSec            float64          `json:"end_sec"`
	Text              string           `json:"text"`
	Words             []TranscriptWord `json:"words"`
	AlignmentReliable bool             `json:"alignment_reliable"`
}
type StreamCommit struct {
	EpisodeNumber       int              `json:"episode_number"`
	Revision            uint64           `json:"revision"`
	CommittedThroughSec float64          `json:"committed_through_sec"`
	Items               []TranscriptItem `json:"items"`
	Final               bool             `json:"final"`
}
type AudioInterval struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
}
type OverlapInterval struct {
	AudioInterval
	Confidence float64 `json:"confidence"`
}
type VADStatus struct {
	Backend  string `json:"backend"`
	Reliable bool   `json:"reliable"`
}
type LoudnessMetrics struct {
	Available bool     `json:"available"`
	MeanDBFS  *float64 `json:"mean_dbfs,omitempty"`
	PeakDBFS  *float64 `json:"peak_dbfs,omitempty"`
	P90DBFS   *float64 `json:"p90_dbfs,omitempty"`
	ChangeDB  *float64 `json:"change_db,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
type SpeechRateMetrics struct {
	Available         bool     `json:"available"`
	MeanUnitsPerSec   *float64 `json:"mean_units_per_sec,omitempty"`
	MaxUnitsPerSec    *float64 `json:"max_units_per_sec,omitempty"`
	ChangeUnitsPerSec *float64 `json:"change_units_per_sec,omitempty"`
	Reason            string   `json:"reason,omitempty"`
}
type PitchMetrics struct {
	Available bool     `json:"available"`
	MedianHz  *float64 `json:"median_hz,omitempty"`
	P90Hz     *float64 `json:"p90_hz,omitempty"`
	ChangeHz  *float64 `json:"change_hz,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
type UnitAudioMetrics struct {
	UnitID     string            `json:"unit_id"`
	StartSec   float64           `json:"start_sec"`
	EndSec     float64           `json:"end_sec"`
	Loudness   LoudnessMetrics   `json:"loudness"`
	SpeechRate SpeechRateMetrics `json:"speech_rate"`
	Pitch      PitchMetrics      `json:"pitch"`
}
type SoundEvent struct {
	Type       string  `json:"type"`
	StartSec   float64 `json:"start_sec"`
	EndSec     float64 `json:"end_sec"`
	Confidence float64 `json:"confidence"`
}
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

// ScoreInput is one complete scoring candidate, not an array of single scores.
// Legacy inline images are intentionally replaced by VisionInput.AssetID.
type ScoreInput struct {
	RequestID            string                  `json:"request_id,omitempty"`
	TaskID               string                  `json:"task_id,omitempty"`
	AnalysisChain        string                  `json:"analysis_chain,omitempty"`
	AudioFeatures        *CandidateAudioFeatures `json:"audio_features,omitempty"`
	EpisodeNumber        int                     `json:"episode"`
	CandidateIndex       int                     `json:"candidate_index"`
	StartSec             float64                 `json:"candidate_start_sec"`
	EndSec               float64                 `json:"candidate_end_sec"`
	SourceSignals        []string                `json:"source_signals,omitempty"`
	EligibleUnits        []SpeechUnit            `json:"eligible_units,omitempty"`
	ContextUnits         []SpeechUnit            `json:"context_units,omitempty"`
	AudioSummary         string                  `json:"audio_summary,omitempty"`
	ShotSummary          string                  `json:"shot_summary,omitempty"`
	MinHighlightScore    float64                 `json:"min_highlight_score"`
	MinCompletenessScore float64                 `json:"min_completeness_score"`
}
type RawEvent struct {
	HighlightScore       float64  `json:"highlight_score"`
	ConflictScore        float64  `json:"conflict_score"`
	EmotionScore         float64  `json:"emotion_score"`
	ReversalScore        float64  `json:"reversal_score"`
	VisualScore          float64  `json:"visual_score"`
	HookScore            float64  `json:"hook_score"`
	CompletenessScore    float64  `json:"completeness_score"`
	StartUnitID          *string  `json:"start_unit_id"`
	EndUnitID            *string  `json:"end_unit_id"`
	RecommendedStartSec  *float64 `json:"recommended_start_sec"`
	RecommendedEndSec    *float64 `json:"recommended_end_sec"`
	Title                string   `json:"title"`
	Reason               string   `json:"reason"`
	Tags                 []string `json:"tags"`
	IncludeInCompilation bool     `json:"include_in_compilation"`
}

// decodeStrict validates tokens before typed decoding: encoding/json alone
// replaces malformed UTF-8, accepts duplicate keys, null scalars and folded keys.
// Only the four nullable RawEvent references may explicitly be null.
func decodeStrict(data []byte, out any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid utf8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := scanJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing json")
	}
	if err := exactJSON(data, reflect.TypeOf(out).Elem(), false, 0); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func scanJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("json depth")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := k.(string)
			if !ok || seen[strings.ToLower(s)] {
				return fmt.Errorf("duplicate key")
			}
			seen[strings.ToLower(s)] = true
			if err = scanJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := scanJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("delimiter")
	}
	_, err = d.Token()
	return err
}
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			for n, v := range jsonFields(f.Type) {
				fields[n] = v
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}
func exactJSON(data []byte, t reflect.Type, nullable bool, depth int) error {
	if depth > 64 {
		return fmt.Errorf("json depth")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if nullable {
			return nil
		}
		return fmt.Errorf("null")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			return err
		}
		fields := jsonFields(t)
		for k, v := range obj {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("unknown field")
			}
			allow := t == reflect.TypeOf(RawEvent{}) && (k == "start_unit_id" || k == "end_unit_id" || k == "recommended_start_sec" || k == "recommended_end_sec")
			if err := exactJSON(v, ft, allow, depth+1); err != nil {
				return err
			}
		}
		if t == reflect.TypeOf(RawEvent{}) || t == reflect.TypeOf(integration.RuntimeStreamOptions{}) {
			for k := range fields {
				if _, ok := obj[k]; !ok {
					return fmt.Errorf("missing contract field")
				}
			}
		}
	case reflect.Slice:
		var a []json.RawMessage
		if err := json.Unmarshal(data, &a); err != nil {
			return err
		}
		for _, v := range a {
			if err := exactJSON(v, t.Elem(), false, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// ScoreInputFromJSON converts the public scoring wire representation without
// importing the model execution stack. Local-only fields must be checked by the
// adapter before marshaling CandidateInput (json:"-" fields cannot be inspected
// here). Inline images and path/URL keys are rejected, never silently dropped.
func ScoreInputFromJSON(data []byte) (ScoreInput, error) {
	var s ScoreInput
	if len(data) > MaxBodyBytes {
		return s, failure(413, "center_request_too_large")
	}
	if decodeStrict(data, &s) != nil || !s.valid() {
		return ScoreInput{}, failure(422, "center_request_invalid")
	}
	return s, nil
}

// RawEventsFromJSON is the explicit strict conversion of scoring RawEvent DTOs.
// Invalid/nil event slots must not be promoted to successful model output.
func RawEventsFromJSON(data []byte, input ScoreInput) ([]*RawEvent, error) {
	var events []*RawEvent
	if len(data) > MaxResultBytes {
		return nil, failure(413, "center_result_too_large")
	}
	if decodeStrict(data, &events) != nil || len(events) > 128 {
		return nil, failure(422, "center_result_invalid")
	}
	for _, e := range events {
		if !eventValid(e, input) {
			return nil, failure(422, "center_result_invalid")
		}
	}
	return events, nil
}

// StreamCommitFromJSON converts transcript.StreamCommit, including word-level
// probability and source-time offset validation. previous is the last delta.
func StreamCommitFromJSON(data []byte, input TranscribeInput, previous *StreamCommit) (StreamCommit, error) {
	var c StreamCommit
	if len(data) > MaxResultBytes {
		return c, failure(413, "center_result_too_large")
	}
	if decodeStrict(data, &c) != nil || !commitValid(c, &input, previous) {
		return StreamCommit{}, failure(422, "center_stream_invalid")
	}
	return c, nil
}

func validUTF8Value(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer:
		return v.IsNil() || validUTF8Value(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !validUTF8Value(v.Field(i)) {
				return false
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if !validUTF8Value(v.Index(i)) {
				return false
			}
		}
	case reflect.String:
		return utf8.ValidString(v.String())
	}
	return true
}
func cloneCommit(c StreamCommit) StreamCommit {
	raw, _ := json.Marshal(c)
	var out StreamCommit
	_ = json.Unmarshal(raw, &out)
	return out
}
func validRange(a, b float64) bool { return finite(a) && finite(b) && a >= 0 && b > a && b <= 86400 }
func finite(v float64) bool {
	return v == v && v <= 1.7976931348623157e308 && v >= -1.7976931348623157e308
}
func textValid(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= 8192
}
func wordsValid(words []TranscriptWord, start, end float64) bool {
	last := start
	for _, w := range words {
		if !validRange(w.StartSec, w.EndSec) || w.StartSec < last || w.EndSec > end || !textValid(w.Text) || w.Probability != nil && !unit(*w.Probability) {
			return false
		}
		last = w.EndSec
	}
	return true
}
func itemsValid(items []TranscriptItem, start, end float64) bool {
	if len(items) > 16384 {
		return false
	}
	last := start
	for _, i := range items {
		if !validRange(i.StartSec, i.EndSec) || i.StartSec < last || i.EndSec > end || !textValid(i.Text) || i.Probability != nil && !unit(*i.Probability) || !wordsValid(i.Words, i.StartSec, i.EndSec) {
			return false
		}
		last = i.EndSec
	}
	return true
}
func commitValid(c StreamCommit, input *TranscribeInput, previous *StreamCommit) bool {
	offset := float64(input.OffsetMS) / 1000
	if !input.Stream || c.EpisodeNumber != input.EpisodeNumber || c.Revision == 0 || !finite(c.CommittedThroughSec) || c.CommittedThroughSec < offset || c.CommittedThroughSec > 86400 || !itemsValid(c.Items, offset, c.CommittedThroughSec) {
		return false
	}
	if previous != nil {
		if previous.Final || c.Revision <= previous.Revision || c.CommittedThroughSec < previous.CommittedThroughSec {
			return false
		}
		for _, i := range c.Items {
			if i.StartSec < previous.CommittedThroughSec {
				return false
			}
		}
	}
	return true
}
func (s ScoreInput) valid() bool {
	if s.EpisodeNumber < 1 || s.CandidateIndex < 0 || !validRange(s.StartSec, s.EndSec) || !unit(s.MinHighlightScore) || !unit(s.MinCompletenessScore) || len(s.EligibleUnits) > 1024 || len(s.ContextUnits) > 1024 || len(s.AudioSummary) > 8192 || len(s.ShotSummary) > 8192 {
		return false
	}
	if s.RequestID != "" && !opaque(s.RequestID) || s.TaskID != "" && !opaque(s.TaskID) {
		return false
	}
	ids := map[string]bool{}
	for _, units := range [][]SpeechUnit{s.EligibleUnits, s.ContextUnits} {
		last := -1.0
		for _, u := range units {
			if !safeID(u.ID) || ids[u.ID] || !validRange(u.StartSec, u.EndSec) || u.StartSec < last || !textValid(u.Text) || !wordsValid(u.Words, u.StartSec, u.EndSec) {
				return false
			}
			ids[u.ID] = true
			last = u.StartSec
		}
	}
	for _, u := range s.EligibleUnits {
		if u.StartSec < s.StartSec || u.EndSec > s.EndSec {
			return false
		}
	}
	if len(s.SourceSignals) > 128 {
		return false
	}
	for _, v := range s.SourceSignals {
		if !safeID(v) {
			return false
		}
	}
	switch s.AnalysisChain {
	case "", "legacy_contact_sheet_v1":
		return s.AudioFeatures == nil
	case "audio_text_v1", "audio_text_stream_v1":
		return s.AudioFeatures != nil && s.AudioSummary == "" && s.ShotSummary == "" && s.AudioFeatures.valid(s)
	default:
		return false
	}
}
func safeID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._~-", r)) {
			return false
		}
	}
	return true
}
func metricsValid(l LoudnessMetrics, s SpeechRateMetrics, p PitchMetrics) bool {
	return metricValid(l.Available, l.Reason, l.MeanDBFS, l.PeakDBFS, l.P90DBFS, l.ChangeDB) && metricValid(s.Available, s.Reason, s.MeanUnitsPerSec, s.MaxUnitsPerSec, s.ChangeUnitsPerSec) && metricValid(p.Available, p.Reason, p.MedianHz, p.P90Hz, p.ChangeHz)
}
func metricValid(available bool, reason string, values ...*float64) bool {
	if available {
		if reason != "" {
			return false
		}
		for _, v := range values {
			if v == nil || !finite(*v) {
				return false
			}
		}
		return true
	}
	switch reason {
	case "analysis_unavailable", "context_unavailable", "empty_text", "incomplete_unit_metrics", "insufficient_samples", "insufficient_speech", "insufficient_voiced_frames", "metrics_unavailable", "no_eligible_units", "no_speech", "transcript_unavailable", "vad_unreliable", "zero_speech_duration":
	default:
		return false
	}
	for _, v := range values {
		if v != nil {
			return false
		}
	}
	return true
}
func (f CandidateAudioFeatures) valid(s ScoreInput) bool {
	if f.Schema != "audio_features_v1" || !validRange(f.Window.StartSec, f.Window.EndSec) || f.Window.StartSec > s.StartSec || f.Window.EndSec < s.EndSec || !safeID(f.VAD.Backend) || !metricsValid(f.Loudness, f.SpeechRate, f.Pitch) {
		return false
	}
	if f.Speech == nil || f.Silence == nil || f.Pauses == nil || f.Overlaps == nil || f.UnitMetrics == nil || f.SoundEvents == nil || len(f.Overlaps) > 2048 || len(f.UnitMetrics) > 1024 || len(f.SoundEvents) > 256 {
		return false
	}
	inside := func(a, b float64) bool { return validRange(a, b) && a >= f.Window.StartSec && b <= f.Window.EndSec }
	for _, units := range [][]SpeechUnit{s.EligibleUnits, s.ContextUnits} {
		for _, u := range units {
			if !inside(u.StartSec, u.EndSec) {
				return false
			}
		}
	}
	for _, intervals := range [][]AudioInterval{f.Speech, f.Silence, f.Pauses} {
		if len(intervals) > 2048 {
			return false
		}
		last := f.Window.StartSec
		for _, i := range intervals {
			if !inside(i.StartSec, i.EndSec) || i.StartSec < last {
				return false
			}
			last = i.EndSec
		}
	}
	last := -1.0
	end := -1.0
	for _, o := range f.Overlaps {
		if !inside(o.StartSec, o.EndSec) || !unit(o.Confidence) || o.StartSec < last || o.StartSec == last && o.EndSec <= end {
			return false
		}
		last = o.StartSec
		end = o.EndSec
	}
	seen := map[string]bool{}
	last = -1
	end = -1
	lastID := ""
	for _, m := range f.UnitMetrics {
		found := false
		for _, u := range s.EligibleUnits {
			if u.ID == m.UnitID && u.StartSec == m.StartSec && u.EndSec == m.EndSec {
				found = true
			}
		}
		if !found || seen[m.UnitID] || !metricsValid(m.Loudness, m.SpeechRate, m.Pitch) || m.StartSec < last || m.StartSec == last && (m.EndSec < end || m.EndSec == end && m.UnitID < lastID) {
			return false
		}
		seen[m.UnitID] = true
		last = m.StartSec
		end = m.EndSec
		lastID = m.UnitID
	}
	last = -1
	end = -1
	for _, e := range f.SoundEvents {
		if e.Type != "laughter" && e.Type != "applause" && e.Type != "music_climax" || !inside(e.StartSec, e.EndSec) || !unit(e.Confidence) || e.StartSec < last || e.StartSec == last && e.EndSec < end {
			return false
		}
		last = e.StartSec
		end = e.EndSec
	}
	return true
}
func eventValid(e *RawEvent, s ScoreInput) bool {
	if e == nil {
		return false
	}
	for _, v := range []float64{e.HighlightScore, e.ConflictScore, e.EmotionScore, e.ReversalScore, e.VisualScore, e.HookScore, e.CompletenessScore} {
		if !unit(v) {
			return false
		}
	}
	if !textValid(e.Title) || !textValid(e.Reason) || e.Tags == nil || len(e.Tags) > 128 {
		return false
	}
	for _, v := range e.Tags {
		if !textValid(v) {
			return false
		}
	}
	if (e.StartUnitID == nil) != (e.EndUnitID == nil) || (e.RecommendedStartSec == nil) != (e.RecommendedEndSec == nil) {
		return false
	}
	if e.StartUnitID != nil {
		a, b := -1, -1
		for i, u := range s.EligibleUnits {
			if u.ID == *e.StartUnitID {
				a = i
			}
			if u.ID == *e.EndUnitID {
				b = i
			}
		}
		if a < 0 || b < a {
			return false
		}
	}
	if e.RecommendedStartSec != nil {
		if !validRange(*e.RecommendedStartSec, *e.RecommendedEndSec) || *e.RecommendedStartSec < s.StartSec || *e.RecommendedEndSec > s.EndSec {
			return false
		}
	}
	return true
}
