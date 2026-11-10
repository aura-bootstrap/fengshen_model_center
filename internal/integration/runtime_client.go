package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const runtimeBodyLimit = 64 << 10
const runtimeResultLimit = 4 << 20

var ErrRuntimeEventCursorExpired = errors.New("center_event_cursor_expired")
var errRuntimeInvalid = errors.New("center_result_invalid")

// RuntimeHTTPError deliberately retains no server text, request URL or secrets.
type RuntimeHTTPError struct {
	Status int
	Code   string
}

func (e *RuntimeHTTPError) Error() string { return e.Code }
func (e *RuntimeHTTPError) Is(target error) bool {
	return target == ErrRuntimeEventCursorExpired && e.Status == 410 && e.Code == ErrRuntimeEventCursorExpired.Error()
}

// RuntimeClient uses a non-secret task scope established by the caller.
// It never opens sessions, retries writes, or cancels a job on connection loss.
type RuntimeClient struct {
	client *Client
	http   *http.Client
}

func NewRuntimeClient(c *Client) *RuntimeClient {
	h := SessionHTTPClient(30 * time.Second)
	h.Timeout = 0 // SSE and media lifetime is controlled by the caller's context.
	h.Transport.(*http.Transport).DisableCompression = true
	return &RuntimeClient{client: c, http: h}
}
func (c *RuntimeClient) CloseIdleConnections() { c.http.CloseIdleConnections() }

// RuntimeSessionAddress returns the live literal-loopback center address.
func (c *Client) RuntimeSessionAddress(s *RuntimeSessionHandle) (string, error) {
	if c == nil || s == nil {
		return "", errors.New("center_session_invalid")
	}
	return c.address(context.Background())
}
func (c *RuntimeClient) request(ctx context.Context, s *RuntimeSessionHandle, method, path string, body io.Reader, n int64, typ string, cursor string) (*http.Response, error) {
	if c == nil || c.client == nil || c.http == nil || s == nil {
		return nil, errors.New("center_session_invalid")
	}
	address, e := c.client.address(ctx)
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, method, "http://"+address+path, body)
	if e != nil {
		return nil, errors.New("center_request_invalid")
	}
	r.ContentLength = n
	// No GetBody or idempotency header: even an explicitly repeated request_id
	// must not cause net/http to replay a write after an uncertain connection.
	r.GetBody = nil
	if typ != "" {
		r.Header.Set("Content-Type", typ)
	}
	if cursor != "" {
		r.Header.Set("Last-Event-ID", cursor)
	}
	if e = c.client.SetSessionHeaders(s, r); e != nil {
		return nil, e
	}
	response, e := c.http.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("center_transport_failed")
	}
	if len(response.Header.Values("Content-Encoding")) != 0 {
		response.Body.Close()
		return nil, errRuntimeInvalid
	}
	return response, nil
}
func runtimeRead(response *http.Response, limit int64) ([]byte, error) {
	if response.ContentLength > limit || response.ContentLength < -1 {
		return nil, errRuntimeInvalid
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if e != nil || int64(len(raw)) > limit || response.ContentLength >= 0 && int64(len(raw)) != response.ContentLength {
		return nil, errRuntimeInvalid
	}
	return raw, nil
}
func runtimeResponseError(response *http.Response) error {
	raw, e := runtimeRead(response, runtimeBodyLimit)
	var out struct {
		Code string `json:"code"`
	}
	code := "center_http_error"
	if e == nil && runtimeDecode(raw, &out) == nil && strings.HasPrefix(out.Code, "center_") && safeCode(out.Code) {
		code = out.Code
	}
	if response.StatusCode == 410 {
		code = ErrRuntimeEventCursorExpired.Error()
	}
	return &RuntimeHTTPError{response.StatusCode, code}
}
func runtimeJSONResponse(response *http.Response, out any, limit int64, statuses ...int) error {
	defer response.Body.Close()
	ok := false
	for _, status := range statuses {
		ok = ok || response.StatusCode == status
	}
	if !ok {
		return runtimeResponseError(response)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		return errRuntimeInvalid
	}
	raw, e := runtimeRead(response, limit)
	if e != nil {
		return e
	}
	return runtimeDecode(raw, out)
}

// Runtime wire DTOs intentionally do not depend on the center's execution stack.
type RuntimeState string

func (s RuntimeState) Terminal() bool { return s == "succeeded" || s == "failed" || s == "cancelled" }
func runtimeStateValid(s RuntimeState) bool {
	return s.Terminal() || s == "queued" || s == "loading" || s == "running"
}

// Snapshot and Event have capitalized Go field names on the existing HTTP wire.
type RuntimeSnapshot struct {
	ID, SessionID, TaskID, RequestID string
	Binding                          ExecutionBindingRef
	BodySHA256                       string
	State                            RuntimeState
	Code                             string
	CreatedAt, StartedAt, FinishedAt time.Time
	Cursor                           uint64
	Acknowledged                     bool
}
type RuntimeEvent struct {
	Sequence uint64
	At       time.Time
	State    RuntimeState
	Code     string
	Progress *RuntimeProgress
	Commit   *RuntimeStreamCommit
}
type RuntimeProgress struct {
	Stage     string               `json:"stage"`
	Completed int                  `json:"completed"`
	Total     int                  `json:"total"`
	Commit    *RuntimeStreamCommit `json:"commit,omitempty"`
}

// RuntimeStreamOptions mirrors transcript.StreamOptions after normalization.
// An explicit wire value must be complete and valid; only nil selects defaults.
// Keep this lightweight contract independent of the model execution stack.
type RuntimeStreamOptions struct {
	WindowSec             float64 `json:"window_sec"`
	StepSec               float64 `json:"step_sec"`
	StableRounds          int     `json:"stable_rounds"`
	TimestampToleranceSec float64 `json:"timestamp_tolerance_sec"`
	FinalizeDelaySec      float64 `json:"finalize_delay_sec"`
}

func (o RuntimeStreamOptions) Validate() error {
	if !runtimeFinite(o.WindowSec) || o.WindowSec < 5 || o.WindowSec > 120 ||
		!runtimeFinite(o.StepSec) || o.StepSec < .5 || o.StepSec > 10 || o.StepSec > o.WindowSec ||
		o.StableRounds < 1 || o.StableRounds > 5 ||
		!runtimeFinite(o.TimestampToleranceSec) || o.TimestampToleranceSec < 0 || o.TimestampToleranceSec > 2 ||
		!runtimeFinite(o.FinalizeDelaySec) || o.FinalizeDelaySec < 0 || o.FinalizeDelaySec > 10 {
		return errors.New("center_request_invalid")
	}
	return nil
}

type RuntimeTranscribeInput struct {
	AssetID       string                `json:"asset_id"`
	Stream        bool                  `json:"stream"`
	VAD           bool                  `json:"vad"`
	OffsetMS      int64                 `json:"offset_ms"`
	EpisodeNumber int                   `json:"episode_number"`
	Options       *RuntimeStreamOptions `json:"options,omitempty"`
}
type RuntimeVisionInput struct {
	AssetID   string            `json:"asset_id"`
	Candidate RuntimeScoreInput `json:"candidate"`
}
type RuntimeAssetInput struct {
	AssetID string `json:"asset_id"`
}
type RuntimeConvertInput struct {
	AssetID          string `json:"asset_id"`
	ReferenceAssetID string `json:"reference_asset_id"`
	Pitch            int    `json:"pitch"`
}
type RuntimeBody struct {
	Component  ExecutionComponentRef   `json:"component"`
	Transcribe *RuntimeTranscribeInput `json:"transcribe,omitempty"`
	Score      *RuntimeScoreInput      `json:"score,omitempty"`
	Vision     *RuntimeVisionInput     `json:"vision,omitempty"`
	Separate   *RuntimeAssetInput      `json:"separate,omitempty"`
	Convert    *RuntimeConvertInput    `json:"convert,omitempty"`
	Face       *RuntimeAssetInput      `json:"face,omitempty"`
}
type RuntimeTranscriptWord struct {
	StartSec    float64  `json:"start_sec"`
	EndSec      float64  `json:"end_sec"`
	Text        string   `json:"text"`
	Probability *float64 `json:"probability,omitempty"`
}
type RuntimeTranscriptItem struct {
	StartSec    float64                 `json:"start_sec"`
	EndSec      float64                 `json:"end_sec"`
	Text        string                  `json:"text"`
	Probability *float64                `json:"probability,omitempty"`
	Words       []RuntimeTranscriptWord `json:"words"`
}
type RuntimeSpeechUnit struct {
	ID                string                  `json:"id"`
	StartSec          float64                 `json:"start_sec"`
	EndSec            float64                 `json:"end_sec"`
	Text              string                  `json:"text"`
	Words             []RuntimeTranscriptWord `json:"words"`
	AlignmentReliable bool                    `json:"alignment_reliable"`
}
type RuntimeStreamCommit struct {
	EpisodeNumber       int                     `json:"episode_number"`
	Revision            uint64                  `json:"revision"`
	CommittedThroughSec float64                 `json:"committed_through_sec"`
	Items               []RuntimeTranscriptItem `json:"items"`
	Final               bool                    `json:"final"`
}
type RuntimeAudioInterval struct {
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
}
type RuntimeOverlapInterval struct {
	RuntimeAudioInterval
	Confidence float64 `json:"confidence"`
}
type RuntimeVADStatus struct {
	Backend  string `json:"backend"`
	Reliable bool   `json:"reliable"`
}
type RuntimeLoudnessMetrics struct {
	Available bool     `json:"available"`
	MeanDBFS  *float64 `json:"mean_dbfs,omitempty"`
	PeakDBFS  *float64 `json:"peak_dbfs,omitempty"`
	P90DBFS   *float64 `json:"p90_dbfs,omitempty"`
	ChangeDB  *float64 `json:"change_db,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
type RuntimeSpeechRateMetrics struct {
	Available         bool     `json:"available"`
	MeanUnitsPerSec   *float64 `json:"mean_units_per_sec,omitempty"`
	MaxUnitsPerSec    *float64 `json:"max_units_per_sec,omitempty"`
	ChangeUnitsPerSec *float64 `json:"change_units_per_sec,omitempty"`
	Reason            string   `json:"reason,omitempty"`
}
type RuntimePitchMetrics struct {
	Available bool     `json:"available"`
	MedianHz  *float64 `json:"median_hz,omitempty"`
	P90Hz     *float64 `json:"p90_hz,omitempty"`
	ChangeHz  *float64 `json:"change_hz,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
type RuntimeUnitAudioMetrics struct {
	UnitID     string                   `json:"unit_id"`
	StartSec   float64                  `json:"start_sec"`
	EndSec     float64                  `json:"end_sec"`
	Loudness   RuntimeLoudnessMetrics   `json:"loudness"`
	SpeechRate RuntimeSpeechRateMetrics `json:"speech_rate"`
	Pitch      RuntimePitchMetrics      `json:"pitch"`
}
type RuntimeSoundEvent struct {
	Type       string  `json:"type"`
	StartSec   float64 `json:"start_sec"`
	EndSec     float64 `json:"end_sec"`
	Confidence float64 `json:"confidence"`
}
type RuntimeAudioFeatures struct {
	Schema      string                    `json:"schema"`
	Window      RuntimeAudioInterval      `json:"window"`
	VAD         RuntimeVADStatus          `json:"vad"`
	Speech      []RuntimeAudioInterval    `json:"speech"`
	Silence     []RuntimeAudioInterval    `json:"silence"`
	Pauses      []RuntimeAudioInterval    `json:"pauses"`
	Overlaps    []RuntimeOverlapInterval  `json:"overlaps"`
	Loudness    RuntimeLoudnessMetrics    `json:"loudness"`
	SpeechRate  RuntimeSpeechRateMetrics  `json:"speech_rate"`
	Pitch       RuntimePitchMetrics       `json:"pitch"`
	UnitMetrics []RuntimeUnitAudioMetrics `json:"unit_metrics"`
	SoundEvents []RuntimeSoundEvent       `json:"sound_events"`
}
type RuntimeScoreInput struct {
	RequestID            string                `json:"request_id,omitempty"`
	TaskID               string                `json:"task_id,omitempty"`
	AnalysisChain        string                `json:"analysis_chain,omitempty"`
	AudioFeatures        *RuntimeAudioFeatures `json:"audio_features,omitempty"`
	EpisodeNumber        int                   `json:"episode"`
	CandidateIndex       int                   `json:"candidate_index"`
	StartSec             float64               `json:"candidate_start_sec"`
	EndSec               float64               `json:"candidate_end_sec"`
	SourceSignals        []string              `json:"source_signals,omitempty"`
	EligibleUnits        []RuntimeSpeechUnit   `json:"eligible_units,omitempty"`
	ContextUnits         []RuntimeSpeechUnit   `json:"context_units,omitempty"`
	AudioSummary         string                `json:"audio_summary,omitempty"`
	ShotSummary          string                `json:"shot_summary,omitempty"`
	MinHighlightScore    float64               `json:"min_highlight_score"`
	MinCompletenessScore float64               `json:"min_completeness_score"`
}
type RuntimeRawEvent struct {
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
type RuntimeMediaResult struct {
	ID         string `json:"id"`
	Bytes      string `json:"bytes"`
	SHA256     string `json:"sha256"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	DurationMS int64  `json:"duration_ms"`
}
type RuntimeFaceResult string

const (
	RuntimeFrontal RuntimeFaceResult = "frontal"
	RuntimeProfile RuntimeFaceResult = "profile"
	RuntimeNoFace  RuntimeFaceResult = "no_face"
)

type RuntimeResult struct {
	Component ExecutionComponentRef   `json:"component"`
	Items     []RuntimeTranscriptItem `json:"items,omitempty"`
	Stream    *RuntimeStreamCommit    `json:"stream,omitempty"`
	Events    []*RuntimeRawEvent      `json:"events"`
	Media     []RuntimeMediaResult    `json:"media,omitempty"`
	Face      RuntimeFaceResult       `json:"face,omitempty"`
}
type RuntimeAssetMetadata struct {
	Kind            string  `json:"kind"`
	Bytes           string  `json:"bytes"`
	SHA256          string  `json:"sha256"`
	SampleRate      uint32  `json:"sample_rate,omitempty"`
	Channels        uint16  `json:"channels,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
}
type RuntimeAsset struct {
	ID       string               `json:"asset_id"`
	Metadata RuntimeAssetMetadata `json:"metadata"`
}

// RuntimeJob is an opaque, immutable local receipt binding all subsequent reads
// to the admitted request. Persist only its Snapshot, never RuntimeSessionHandle.
type RuntimeJob struct {
	snapshot RuntimeSnapshot
	body     RuntimeBody
}

func (j *RuntimeJob) Snapshot() RuntimeSnapshot {
	if j == nil {
		return RuntimeSnapshot{}
	}
	return j.snapshot
}

type runtimeValidationRequest struct {
	Binding ExecutionBindingRef
	Body    RuntimeBody
}

func (b RuntimeBody) validate(scene string) bool {
	if b.Component.Validate(scene) != nil {
		return false
	}
	n := 0
	for _, v := range []bool{b.Transcribe != nil, b.Score != nil, b.Vision != nil, b.Separate != nil, b.Convert != nil, b.Face != nil} {
		if v {
			n++
		}
	}
	if n != 1 {
		return false
	}
	switch scene {
	case "highlight.transcribe":
		v := b.Transcribe
		return v != nil && (v.Options == nil || v.Stream && v.Options.Validate() == nil) && opaqueRuntimeID(v.AssetID) && v.EpisodeNumber > 0 && v.OffsetMS >= 0 && v.OffsetMS <= 86400000 && (v.Stream && b.Component.ID == "whisper-stream" || !v.Stream && b.Component.ID == "whisper-batch")
	case "highlight.score":
		return b.Score != nil && b.Score.valid()
	case "highlight.vision":
		return b.Vision != nil && opaqueRuntimeID(b.Vision.AssetID) && b.Vision.Candidate.valid()
	case "timbre.separate":
		return b.Separate != nil && opaqueRuntimeID(b.Separate.AssetID)
	case "timbre.convert":
		return b.Convert != nil && opaqueRuntimeID(b.Convert.AssetID) && opaqueRuntimeID(b.Convert.ReferenceAssetID) && b.Convert.Pitch >= -24 && b.Convert.Pitch <= 24
	case "image.face":
		return b.Face != nil && opaqueRuntimeID(b.Face.AssetID)
	}
	return false
}
func runtimeFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func runtimeUnit(v float64) bool   { return runtimeFinite(v) && v >= 0 && v <= 1 }
func runtimeRange(a, b float64) bool {
	return runtimeFinite(a) && runtimeFinite(b) && a >= 0 && b > a && b <= 86400
}
func runtimeText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= 8192
}
func runtimeWords(words []RuntimeTranscriptWord, start, end float64) bool {
	last := start
	for _, w := range words {
		if !runtimeRange(w.StartSec, w.EndSec) || w.StartSec < last || w.EndSec > end || !runtimeText(w.Text) || w.Probability != nil && !runtimeUnit(*w.Probability) {
			return false
		}
		last = w.EndSec
	}
	return true
}
func runtimeItems(items []RuntimeTranscriptItem, start, end float64) bool {
	if len(items) > 16384 {
		return false
	}
	last := start
	for _, v := range items {
		if !runtimeRange(v.StartSec, v.EndSec) || v.StartSec < last || v.EndSec > end || !runtimeText(v.Text) || v.Probability != nil && !runtimeUnit(*v.Probability) || !runtimeWords(v.Words, v.StartSec, v.EndSec) {
			return false
		}
		last = v.EndSec
	}
	return true
}
func runtimeCommit(v RuntimeStreamCommit, input *RuntimeTranscribeInput, previous *RuntimeStreamCommit) bool {
	if input == nil || !input.Stream || v.EpisodeNumber != input.EpisodeNumber || v.Revision == 0 || !runtimeFinite(v.CommittedThroughSec) || v.CommittedThroughSec < float64(input.OffsetMS)/1000 || v.CommittedThroughSec > 86400 || !runtimeItems(v.Items, float64(input.OffsetMS)/1000, v.CommittedThroughSec) {
		return false
	}
	if previous != nil {
		if previous.Final || v.Revision <= previous.Revision || v.CommittedThroughSec < previous.CommittedThroughSec {
			return false
		}
		for _, i := range v.Items {
			if i.StartSec < previous.CommittedThroughSec {
				return false
			}
		}
	}
	return true
}
func (s RuntimeScoreInput) valid() bool {
	if s.EpisodeNumber < 1 || s.CandidateIndex < 0 || !runtimeRange(s.StartSec, s.EndSec) || !runtimeUnit(s.MinHighlightScore) || !runtimeUnit(s.MinCompletenessScore) || len(s.EligibleUnits) > 1024 || len(s.ContextUnits) > 1024 || len(s.AudioSummary) > 8192 || len(s.ShotSummary) > 8192 || s.RequestID != "" && !opaqueRuntimeID(s.RequestID) || s.TaskID != "" && !opaqueRuntimeID(s.TaskID) || len(s.SourceSignals) > 128 {
		return false
	}
	ids := map[string]bool{}
	for _, units := range [][]RuntimeSpeechUnit{s.EligibleUnits, s.ContextUnits} {
		last := -1.0
		for _, u := range units {
			if !runtimeSafeID(u.ID) || ids[u.ID] || !runtimeRange(u.StartSec, u.EndSec) || u.StartSec < last || !runtimeText(u.Text) || !runtimeWords(u.Words, u.StartSec, u.EndSec) {
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
	for _, v := range s.SourceSignals {
		if !runtimeSafeID(v) {
			return false
		}
	}
	switch s.AnalysisChain {
	case "", "legacy_contact_sheet_v1":
		return s.AudioFeatures == nil
	case "audio_text_v1", "audio_text_stream_v1":
		return s.AudioFeatures != nil && s.AudioSummary == "" && s.ShotSummary == "" && s.AudioFeatures.valid(s)
	}
	return false
}
func runtimeSafeID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._~-", c)) {
			return false
		}
	}
	return true
}
func runtimeMetric(available bool, reason string, values ...*float64) bool {
	if available {
		if reason != "" {
			return false
		}
		for _, v := range values {
			if v == nil || !runtimeFinite(*v) {
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
func runtimeMetrics(l RuntimeLoudnessMetrics, s RuntimeSpeechRateMetrics, p RuntimePitchMetrics) bool {
	return runtimeMetric(l.Available, l.Reason, l.MeanDBFS, l.PeakDBFS, l.P90DBFS, l.ChangeDB) && runtimeMetric(s.Available, s.Reason, s.MeanUnitsPerSec, s.MaxUnitsPerSec, s.ChangeUnitsPerSec) && runtimeMetric(p.Available, p.Reason, p.MedianHz, p.P90Hz, p.ChangeHz)
}
func (f RuntimeAudioFeatures) valid(s RuntimeScoreInput) bool {
	if f.Schema != "audio_features_v1" || !runtimeRange(f.Window.StartSec, f.Window.EndSec) || f.Window.StartSec > s.StartSec || f.Window.EndSec < s.EndSec || !runtimeSafeID(f.VAD.Backend) || !runtimeMetrics(f.Loudness, f.SpeechRate, f.Pitch) || f.Speech == nil || f.Silence == nil || f.Pauses == nil || f.Overlaps == nil || f.UnitMetrics == nil || f.SoundEvents == nil || len(f.Overlaps) > 2048 || len(f.UnitMetrics) > 1024 || len(f.SoundEvents) > 256 {
		return false
	}
	inside := func(a, b float64) bool { return runtimeRange(a, b) && a >= f.Window.StartSec && b <= f.Window.EndSec }
	for _, units := range [][]RuntimeSpeechUnit{s.EligibleUnits, s.ContextUnits} {
		for _, u := range units {
			if !inside(u.StartSec, u.EndSec) {
				return false
			}
		}
	}
	for _, list := range [][]RuntimeAudioInterval{f.Speech, f.Silence, f.Pauses} {
		if len(list) > 2048 {
			return false
		}
		last := f.Window.StartSec
		for _, v := range list {
			if !inside(v.StartSec, v.EndSec) || v.StartSec < last {
				return false
			}
			last = v.EndSec
		}
	}
	last, end := -1.0, -1.0
	for _, v := range f.Overlaps {
		if !inside(v.StartSec, v.EndSec) || !runtimeUnit(v.Confidence) || v.StartSec < last || v.StartSec == last && v.EndSec <= end {
			return false
		}
		last, end = v.StartSec, v.EndSec
	}
	last, end = -1, -1
	lastID := ""
	seen := map[string]bool{}
	for _, v := range f.UnitMetrics {
		found := false
		for _, u := range s.EligibleUnits {
			if u.ID == v.UnitID && u.StartSec == v.StartSec && u.EndSec == v.EndSec {
				found = true
			}
		}
		if !found || seen[v.UnitID] || !runtimeMetrics(v.Loudness, v.SpeechRate, v.Pitch) || v.StartSec < last || v.StartSec == last && (v.EndSec < end || v.EndSec == end && v.UnitID < lastID) {
			return false
		}
		seen[v.UnitID] = true
		last, end, lastID = v.StartSec, v.EndSec, v.UnitID
	}
	last, end = -1, -1
	for _, v := range f.SoundEvents {
		if v.Type != "laughter" && v.Type != "applause" && v.Type != "music_climax" || !inside(v.StartSec, v.EndSec) || !runtimeUnit(v.Confidence) || v.StartSec < last || v.StartSec == last && v.EndSec < end {
			return false
		}
		last, end = v.StartSec, v.EndSec
	}
	return true
}
func (r RuntimeResult) validate(req runtimeValidationRequest) bool {
	if r.Component != req.Body.Component {
		return false
	}
	n := 0
	for _, v := range []bool{r.Items != nil, r.Stream != nil, r.Events != nil, r.Media != nil, r.Face != ""} {
		if v {
			n++
		}
	}
	if n != 1 {
		return false
	}
	switch req.Binding.Scene {
	case "highlight.transcribe":
		if req.Body.Transcribe == nil {
			return false
		}
		if req.Body.Transcribe.Stream {
			return r.Stream != nil && r.Stream.Final && runtimeCommit(*r.Stream, req.Body.Transcribe, nil)
		}
		return r.Items != nil && len(r.Items) > 0 && runtimeItems(r.Items, float64(req.Body.Transcribe.OffsetMS)/1000, 86400)
	case "highlight.score", "highlight.vision":
		var s RuntimeScoreInput
		if req.Body.Score != nil {
			s = *req.Body.Score
		} else if req.Body.Vision != nil {
			s = req.Body.Vision.Candidate
		} else {
			return false
		}
		if r.Events == nil || len(r.Events) > 128 {
			return false
		}
		for _, v := range r.Events {
			if !runtimeRawEvent(v, s) {
				return false
			}
		}
		return true
	case "image.face":
		return r.Face == RuntimeFrontal || r.Face == RuntimeProfile || r.Face == RuntimeNoFace
	case "timbre.separate", "timbre.convert":
		want := 1
		if req.Binding.Scene == "timbre.separate" {
			want = 2
		}
		if len(r.Media) != want {
			return false
		}
		seen := map[string]bool{}
		for _, v := range r.Media {
			if !v.valid() || seen[v.ID] {
				return false
			}
			seen[v.ID] = true
		}
		return true
	}
	return false
}
func runtimeRawEvent(e *RuntimeRawEvent, s RuntimeScoreInput) bool {
	if e == nil {
		return false
	}
	for _, v := range []float64{e.HighlightScore, e.ConflictScore, e.EmotionScore, e.ReversalScore, e.VisualScore, e.HookScore, e.CompletenessScore} {
		if !runtimeUnit(v) {
			return false
		}
	}
	if !runtimeText(e.Title) || !runtimeText(e.Reason) || e.Tags == nil || len(e.Tags) > 128 || (e.StartUnitID == nil) != (e.EndUnitID == nil) || (e.RecommendedStartSec == nil) != (e.RecommendedEndSec == nil) {
		return false
	}
	for _, v := range e.Tags {
		if !runtimeText(v) {
			return false
		}
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
	return e.RecommendedStartSec == nil || runtimeRange(*e.RecommendedStartSec, *e.RecommendedEndSec) && *e.RecommendedStartSec >= s.StartSec && *e.RecommendedEndSec <= s.EndSec
}
func (m RuntimeMediaResult) valid() bool {
	n, e := strconv.ParseInt(m.Bytes, 10, 64)
	return e == nil && strconv.FormatInt(n, 10) == m.Bytes && n > 0 && n <= 1<<30 && opaqueRuntimeID(m.ID) && digestValid(m.SHA256) && m.SampleRate >= 8000 && m.SampleRate <= 192000 && m.Channels >= 1 && m.Channels <= 2 && m.DurationMS > 0 && m.DurationMS <= 86400000
}

// runtimeDecode rejects duplicate/folded/unknown keys and bounded-depth malformed
// JSON before decoding. Null is allowed only where the actual server emits it.
func runtimeDecode(raw []byte, out any) error {
	if !utf8.Valid(raw) || len(raw) == 0 || len(raw) > runtimeResultLimit {
		return errRuntimeInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 64 {
			return errRuntimeInvalid
		}
		t, e := d.Token()
		if e != nil {
			return errRuntimeInvalid
		}
		v, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch v {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return errRuntimeInvalid
				}
				s, ok := k.(string)
				s = strings.ToLower(s)
				if !ok || seen[s] {
					return errRuntimeInvalid
				}
				seen[s] = true
				if e = scan(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = scan(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errRuntimeInvalid
		}
		_, e = d.Token()
		return e
	}
	if scan(0) != nil {
		return errRuntimeInvalid
	}
	if _, e := d.Token(); e != io.EOF {
		return errRuntimeInvalid
	}
	if runtimeShape(raw, reflect.TypeOf(out).Elem(), false, 0) != nil {
		return errRuntimeInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return errRuntimeInvalid
	}
	return nil
}
func runtimeFields(t reflect.Type) map[string]reflect.StructField {
	m := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			for k, v := range runtimeFields(f.Type) {
				m[k] = v
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		m[name] = f
	}
	return m
}
func runtimeShape(raw []byte, t reflect.Type, nullable bool, depth int) error {
	if depth > 64 {
		return errRuntimeInvalid
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable || t.Kind() == reflect.Slice {
			return nil
		}
		return errRuntimeInvalid
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Time{}) {
		var v time.Time
		return json.Unmarshal(raw, &v)
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return errRuntimeInvalid
		}
		fields := runtimeFields(t)
		for k, v := range obj {
			f, ok := fields[k]
			if !ok {
				return errRuntimeInvalid
			}
			allow := t == reflect.TypeOf(RuntimeEvent{}) && (k == "Progress" || k == "Commit") || t == reflect.TypeOf(RuntimeResult{}) && k == "events" || t == reflect.TypeOf(RuntimeRawEvent{}) && (k == "start_unit_id" || k == "end_unit_id" || k == "recommended_start_sec" || k == "recommended_end_sec")
			if runtimeShape(v, f.Type, allow, depth+1) != nil {
				return errRuntimeInvalid
			}
		}
		if t == reflect.TypeOf(RuntimeRawEvent{}) || t == reflect.TypeOf(RuntimeStreamOptions{}) {
			for k := range fields {
				if _, ok := obj[k]; !ok {
					return errRuntimeInvalid
				}
			}
		}
	case reflect.Slice:
		var a []json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return errRuntimeInvalid
		}
		for _, v := range a {
			if runtimeShape(v, t.Elem(), false, depth+1) != nil {
				return errRuntimeInvalid
			}
		}
	}
	return nil
}

func runtimeScope(s *RuntimeSessionHandle, b ExecutionBindingRef) bool {
	return s != nil && s.Scope().Allows(s.Scope().TaskID, b)
}
func runtimeSnapshotValid(v RuntimeSnapshot, s *RuntimeSessionHandle, old *RuntimeSnapshot) bool {
	if s == nil {
		return false
	}
	g := s.Scope()
	if !opaqueRuntimeID(v.ID) || !opaqueRuntimeID(v.RequestID) || v.SessionID != g.SessionID || v.TaskID != g.TaskID || !g.Allows(v.TaskID, v.Binding) || !digestValid(v.BodySHA256) || !runtimeStateValid(v.State) || !safeCode(v.Code) || v.CreatedAt.IsZero() || v.Cursor == 0 {
		return false
	}
	if v.State.Terminal() && (v.FinishedAt.IsZero() || v.FinishedAt.Before(v.CreatedAt)) || !v.StartedAt.IsZero() && v.StartedAt.Before(v.CreatedAt) {
		return false
	}
	if v.Acknowledged && v.State != "succeeded" {
		return false
	}
	if old != nil && (old.Acknowledged && !v.Acknowledged || v.ID != old.ID || v.RequestID != old.RequestID || v.Binding != old.Binding || v.BodySHA256 != old.BodySHA256 || v.CreatedAt != old.CreatedAt || v.Cursor < old.Cursor || old.State.Terminal() && v.State != old.State) {
		return false
	}
	return true
}

// Submit never replays an uncertain write. Keep requestID/body and explicitly
// resubmit only after the caller decides to resolve admission uncertainty.
func (c *RuntimeClient) Submit(ctx context.Context, s *RuntimeSessionHandle, requestID string, binding ExecutionBindingRef, body RuntimeBody) (*RuntimeJob, error) {
	if !opaqueRuntimeID(requestID) || !runtimeScope(s, binding) || !body.validate(binding.Scene) {
		return nil, errors.New("center_request_invalid")
	}
	task := s.Scope().TaskID
	if body.Score != nil && body.Score.TaskID != "" && body.Score.TaskID != task || body.Vision != nil && body.Vision.Candidate.TaskID != "" && body.Vision.Candidate.TaskID != task {
		return nil, errors.New("center_asset_forbidden")
	}
	bodyRaw, e := json.Marshal(body)
	if e != nil || len(bodyRaw) > runtimeBodyLimit {
		return nil, errors.New("center_request_too_large")
	}
	var owned RuntimeBody
	if runtimeDecode(bodyRaw, &owned) != nil {
		return nil, errors.New("center_request_invalid")
	}
	raw, _ := json.Marshal(struct {
		RequestID string              `json:"request_id"`
		Binding   ExecutionBindingRef `json:"binding"`
		Body      RuntimeBody         `json:"body"`
	}{requestID, binding, owned})
	if len(raw) > runtimeBodyLimit+1024 {
		return nil, errors.New("center_request_too_large")
	}
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{binding, owned})
	sum := sha256.Sum256(frozen)
	response, e := c.request(ctx, s, "POST", RuntimeJobsPath, bytes.NewReader(raw), int64(len(raw)), "application/json", "")
	if e != nil {
		return nil, e
	}
	var snap RuntimeSnapshot
	if e = runtimeJSONResponse(response, &snap, runtimeBodyLimit, 200, 202); e != nil {
		return nil, e
	}
	if !runtimeSnapshotValid(snap, s, nil) || snap.RequestID != requestID || snap.Binding != binding || snap.BodySHA256 != hex.EncodeToString(sum[:]) {
		return nil, errRuntimeInvalid
	}
	return &RuntimeJob{snap, owned}, nil
}

// Resume binds a persisted, non-secret receipt to a newly verified same-session
// session. The server currently owns jobs by SessionID, not TaskID alone.
func (c *RuntimeClient) Resume(s *RuntimeSessionHandle, snapshot RuntimeSnapshot, body RuntimeBody) (*RuntimeJob, error) {
	if !runtimeSnapshotValid(snapshot, s, nil) || !body.validate(snapshot.Binding.Scene) {
		return nil, errRuntimeInvalid
	}
	raw, e := json.Marshal(body)
	if e != nil || len(raw) > runtimeBodyLimit {
		return nil, errRuntimeInvalid
	}
	var owned RuntimeBody
	if runtimeDecode(raw, &owned) != nil {
		return nil, errRuntimeInvalid
	}
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{snapshot.Binding, owned})
	sum := sha256.Sum256(frozen)
	if snapshot.BodySHA256 != hex.EncodeToString(sum[:]) {
		return nil, errRuntimeInvalid
	}
	return &RuntimeJob{snapshot, owned}, nil
}
func (c *RuntimeClient) jobSnapshot(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob, cancel bool) (RuntimeSnapshot, error) {
	if j == nil || !runtimeSnapshotValid(j.snapshot, s, nil) {
		return RuntimeSnapshot{}, errRuntimeInvalid
	}
	method, path, typ := "GET", RuntimeJobsPath+"/"+j.snapshot.ID, ""
	var body io.Reader
	var n int64
	if cancel {
		method = "POST"
		path += "/cancel"
		typ = "application/json"
		body = strings.NewReader("{}")
		n = 2
	}
	response, e := c.request(ctx, s, method, path, body, n, typ, "")
	if e != nil {
		return RuntimeSnapshot{}, e
	}
	var v RuntimeSnapshot
	if e = runtimeJSONResponse(response, &v, runtimeBodyLimit, 200); e != nil {
		return RuntimeSnapshot{}, e
	}
	if !runtimeSnapshotValid(v, s, &j.snapshot) {
		return RuntimeSnapshot{}, errRuntimeInvalid
	}
	return v, nil
}
func (c *RuntimeClient) Query(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob) (RuntimeSnapshot, error) {
	return c.jobSnapshot(ctx, s, j, false)
}
func (c *RuntimeClient) Cancel(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob) (RuntimeSnapshot, error) {
	return c.jobSnapshot(ctx, s, j, true)
}

// Acknowledge is explicit: callers must durably persist the complete result first.
// An unknown response never triggers a retry or a new model submission.
func (c *RuntimeClient) Acknowledge(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob) (RuntimeSnapshot, error) {
	if j == nil || !runtimeSnapshotValid(j.snapshot, s, nil) {
		return RuntimeSnapshot{}, errRuntimeInvalid
	}
	response, e := c.request(ctx, s, "POST", RuntimeJobsPath+"/"+j.snapshot.ID+"/ack", strings.NewReader("{}"), 2, "application/json", "")
	if e != nil {
		return RuntimeSnapshot{}, e
	}
	if len(response.Header.Values("Content-Type")) != 1 {
		response.Body.Close()
		return RuntimeSnapshot{}, errRuntimeInvalid
	}
	var v RuntimeSnapshot
	if e = runtimeJSONResponse(response, &v, runtimeBodyLimit, 200); e != nil {
		return RuntimeSnapshot{}, e
	}
	if !runtimeSnapshotValid(v, s, &j.snapshot) || v.State != "succeeded" || !v.State.Terminal() || !v.Acknowledged {
		return RuntimeSnapshot{}, errRuntimeInvalid
	}
	return v, nil
}

// RuntimeCompletion is a non-secret, complete durable result receipt. Commits
// contains every ordered stream delta, not just the final result delta.
type RuntimeCompletion struct {
	Snapshot RuntimeSnapshot
	Body     RuntimeBody
	Result   RuntimeResult
	Commits  []RuntimeStreamCommit `json:"commits,omitempty"`
}

func (r RuntimeCompletion) Validate() error {
	s := r.Snapshot
	if !opaqueRuntimeID(s.ID) || !opaqueRuntimeID(s.SessionID) || !opaqueRuntimeID(s.TaskID) || !opaqueRuntimeID(s.RequestID) || s.Binding.Validate() != nil || !digestValid(s.BodySHA256) || s.State != "succeeded" || s.CreatedAt.IsZero() || s.FinishedAt.Before(s.CreatedAt) || s.FinishedAt.IsZero() || s.Cursor == 0 || !r.Body.validate(s.Binding.Scene) || !r.Result.validate(runtimeValidationRequest{s.Binding, r.Body}) {
		return errRuntimeInvalid
	}
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{s.Binding, r.Body})
	sum := sha256.Sum256(frozen)
	if hex.EncodeToString(sum[:]) != s.BodySHA256 {
		return errRuntimeInvalid
	}
	if r.Result.Stream == nil {
		if len(r.Commits) != 0 {
			return errRuntimeInvalid
		}
	} else {
		if len(r.Commits) == 0 || len(r.Commits) > 16384 {
			return errRuntimeInvalid
		}
		var previous *RuntimeStreamCommit
		var items []RuntimeTranscriptItem
		for i := range r.Commits {
			if !runtimeCommit(r.Commits[i], r.Body.Transcribe, previous) || len(items)+len(r.Commits[i].Items) > 16384 {
				return errRuntimeInvalid
			}
			items = append(items, r.Commits[i].Items...)
			previous = &r.Commits[i]
		}
		// Event commits are deltas; the final result contains every committed item.
		cumulative := *previous
		cumulative.Items = items
		// Match the jobs no-speech rule without normalizing nested words or probabilities.
		if len(items) == 0 && len(r.Result.Stream.Items) == 0 {
			cumulative.Items = r.Result.Stream.Items
		}
		if !previous.Final || !reflect.DeepEqual(&cumulative, r.Result.Stream) {
			return errRuntimeInvalid
		}
	}
	raw, e := json.Marshal(r)
	if e != nil || len(raw) > runtimeResultLimit {
		return errRuntimeInvalid
	}
	return nil
}

func (c *RuntimeClient) Result(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob) (RuntimeResult, error) {
	snap, e := c.Query(ctx, s, j)
	if e != nil {
		return RuntimeResult{}, e
	}
	if snap.State != "succeeded" {
		return RuntimeResult{}, errors.New("center_result_unavailable")
	}
	response, e := c.request(ctx, s, "GET", RuntimeJobsPath+"/"+snap.ID+"/results/"+snap.ID, nil, 0, "", "")
	if e != nil {
		return RuntimeResult{}, e
	}
	var v RuntimeResult
	if e = runtimeJSONResponse(response, &v, runtimeResultLimit, 200); e != nil {
		return RuntimeResult{}, e
	}
	if !v.validate(runtimeValidationRequest{snap.Binding, j.body}) {
		return RuntimeResult{}, errRuntimeInvalid
	}
	return v, nil
}

type runtimeUploadReader struct {
	ctx         context.Context
	src         io.Reader
	left, total int64
	sum         hash.Hash
	err         error
}

func (r *runtimeUploadReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		r.err = e
		return 0, e
	}
	if r.left == 0 {
		var b [1]byte
		n, e := r.src.Read(b[:])
		if n != 0 || e != io.EOF {
			r.err = errors.New("center_length_invalid")
			return 0, r.err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, e := r.src.Read(p)
	r.left -= int64(n)
	r.total += int64(n)
	r.sum.Write(p[:n])
	if e == io.EOF && r.left != 0 {
		r.err = io.ErrUnexpectedEOF
		return n, r.err
	}
	if e != nil && e != io.EOF {
		r.err = e
	}
	return n, e
}
func (c *RuntimeClient) Upload(ctx context.Context, s *RuntimeSessionHandle, contentType string, size int64, src io.Reader) (RuntimeAsset, error) {
	limit := int64(8 << 20)
	kind := "image"
	switch contentType {
	case "audio/wav":
		limit = 1 << 30
		kind = "wav"
	case "image/png", "image/jpeg":
	default:
		return RuntimeAsset{}, errors.New("center_media_type_invalid")
	}
	if size <= 0 || size > limit || src == nil || s == nil || len(s.Scope().Bindings) == 0 {
		return RuntimeAsset{}, errors.New("center_request_invalid")
	}
	reader := &runtimeUploadReader{ctx: ctx, src: src, left: size, sum: sha256.New()}
	response, e := c.request(ctx, s, "POST", RuntimeAssetsPath, reader, size, contentType, "")
	if e != nil {
		return RuntimeAsset{}, e
	}
	var v RuntimeAsset
	if e = runtimeJSONResponse(response, &v, runtimeBodyLimit, 201); e != nil {
		return RuntimeAsset{}, e
	}
	// Some transports do not probe EOF after Content-Length bytes.
	if reader.left == 0 && reader.err == nil {
		var b [1]byte
		_, _ = reader.Read(b[:])
	}
	m := v.Metadata
	if reader.err != nil || reader.total != size || !opaqueRuntimeID(v.ID) || m.Kind != kind || m.Bytes != strconv.FormatInt(size, 10) || m.SHA256 != hex.EncodeToString(reader.sum.Sum(nil)) {
		return RuntimeAsset{}, errRuntimeInvalid
	}
	if kind == "wav" {
		if m.SampleRate < 8000 || m.SampleRate > 192000 || m.Channels < 1 || m.Channels > 8 || !runtimeFinite(m.DurationSeconds) || m.DurationSeconds <= 0 || m.Width != 0 || m.Height != 0 {
			return RuntimeAsset{}, errRuntimeInvalid
		}
	} else if m.Width < 1 || m.Height < 1 || m.Width > 4096 || m.Height > 4096 || m.SampleRate != 0 || m.Channels != 0 || m.DurationSeconds != 0 {
		return RuntimeAsset{}, errRuntimeInvalid
	}
	return v, nil
}

// Events consumes exactly one connection, starting after cursor. An EOF is not
// proof of job success; query the snapshot/result. Callback errors stop reading.
func (c *RuntimeClient) Events(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob, cursor uint64, emit func(RuntimeEvent) error) error {
	if j == nil || emit == nil || !runtimeSnapshotValid(j.snapshot, s, nil) {
		return errRuntimeInvalid
	}
	response, e := c.request(ctx, s, "GET", RuntimeJobsPath+"/"+j.snapshot.ID+"/events", nil, 0, "", strconv.FormatUint(cursor, 10))
	if e != nil {
		return e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return runtimeResponseError(response)
	}
	if response.Header.Get("Content-Type") != "text/event-stream" || response.ContentLength > 0 {
		return errRuntimeInvalid
	}
	var previous *RuntimeStreamCommit
	return runtimeReadEvents(ctx, response.Body, cursor, func(v RuntimeEvent) error {
		if v.Commit != nil {
			if !runtimeCommit(*v.Commit, j.body.Transcribe, previous) {
				return errRuntimeInvalid
			}
			raw, _ := json.Marshal(v.Commit)
			var owned RuntimeStreamCommit
			if runtimeDecode(raw, &owned) != nil {
				return errRuntimeInvalid
			}
			previous = &owned
		}
		return emit(v)
	})
}
func runtimeReadEvents(ctx context.Context, src io.Reader, cursor uint64, emit func(RuntimeEvent) error) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), runtimeResultLimit+128)
	id, kind, data := "", "", ""
	frameBytes := 0
	terminal := false
	for scanner.Scan() {
		if e := ctx.Err(); e != nil {
			return e
		}
		line := scanner.Text()
		frameBytes += len(line) + 1
		if frameBytes > runtimeResultLimit+128 {
			return errRuntimeInvalid
		}
		if line == "" {
			if id == "" && kind == "" && data == "" {
				frameBytes = 0
				continue
			}
			n, e := strconv.ParseUint(id, 10, 64)
			if e != nil || strconv.FormatUint(n, 10) != id || cursor == ^uint64(0) || n != cursor+1 || kind != "runtime" || data == "" || terminal {
				return errRuntimeInvalid
			}
			var v RuntimeEvent
			if runtimeDecode([]byte(data), &v) != nil || v.Sequence != n || v.At.IsZero() || !runtimeStateValid(v.State) || !safeCode(v.Code) {
				return errRuntimeInvalid
			}
			if v.Progress != nil {
				p := v.Progress
				if p.Stage != "loading" && p.Stage != "running" || p.Completed < 0 || p.Total < 0 || p.Completed > p.Total || p.Commit != nil {
					return errRuntimeInvalid
				}
			}
			if e = emit(v); e != nil {
				return e
			}
			cursor = n
			terminal = v.State.Terminal()
			id, kind, data = "", "", ""
			frameBytes = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return errRuntimeInvalid
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "id":
			if id != "" {
				return errRuntimeInvalid
			}
			id = value
		case "event":
			if kind != "" {
				return errRuntimeInvalid
			}
			kind = value
		case "data":
			if data != "" {
				return errRuntimeInvalid
			}
			data = value
		default:
			return errRuntimeInvalid
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanner.Err() != nil || id != "" || kind != "" || data != "" {
		return errRuntimeInvalid
	}
	return nil
}

// DownloadWAV writes only a validated result from this job. dst must be a new,
// caller-owned task file: on any error discard its partial bytes. No paths are
// accepted or created here. The whole WAV is validated with bounded memory.
func (c *RuntimeClient) DownloadWAV(ctx context.Context, s *RuntimeSessionHandle, j *RuntimeJob, resultID string, dst io.Writer) error {
	if dst == nil || !opaqueRuntimeID(resultID) {
		return errRuntimeInvalid
	}
	result, e := c.Result(ctx, s, j)
	if e != nil {
		return e
	}
	var media *RuntimeMediaResult
	for i := range result.Media {
		if result.Media[i].ID == resultID {
			media = &result.Media[i]
		}
	}
	if media == nil || !media.valid() {
		return errRuntimeInvalid
	}
	response, e := c.request(ctx, s, "GET", RuntimeJobsPath+"/"+j.snapshot.ID+"/results/"+resultID, nil, 0, "", "")
	if e != nil {
		return e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return runtimeResponseError(response)
	}
	size, _ := strconv.ParseInt(media.Bytes, 10, 64)
	if response.Header.Get("Content-Type") != "audio/wav" || response.ContentLength != size || len(response.Header.Values("X-Content-SHA256")) != 1 || response.Header.Get("X-Content-SHA256") != media.SHA256 {
		return errRuntimeInvalid
	}
	h := sha256.New()
	limited := io.LimitReader(response.Body, size+1)
	reader := io.TeeReader(&runtimeContextReader{ctx, limited}, io.MultiWriter(dst, h))
	if e = runtimeWAV(reader, size, *media); e != nil {
		return e
	}
	var extra [1]byte
	n, e := reader.Read(extra[:])
	if n != 0 || e != io.EOF || hex.EncodeToString(h.Sum(nil)) != media.SHA256 {
		return errRuntimeInvalid
	}
	return nil
}

type runtimeContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *runtimeContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func runtimeWAV(r io.Reader, size int64, m RuntimeMediaResult) error {
	var header [12]byte
	if size < 44 {
		return errRuntimeInvalid
	}
	if _, e := io.ReadFull(r, header[:]); e != nil {
		return errRuntimeInvalid
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(header[4:]))+8 != size {
		return errRuntimeInvalid
	}
	haveFmt, haveData := false, false
	var align uint16
	var rate uint32
	var data int64
	for pos := int64(12); pos < size; {
		var chunk [8]byte
		if size-pos < 8 {
			return errRuntimeInvalid
		}
		if _, e := io.ReadFull(r, chunk[:]); e != nil {
			return errRuntimeInvalid
		}
		n := int64(binary.LittleEndian.Uint32(chunk[4:]))
		pos += 8
		if n > size-pos {
			return errRuntimeInvalid
		}
		switch string(chunk[:4]) {
		case "fmt ":
			if haveFmt || haveData || n != 16 && n != 18 {
				return errRuntimeInvalid
			}
			var f [18]byte
			if _, e := io.ReadFull(r, f[:n]); e != nil {
				return errRuntimeInvalid
			}
			channels := binary.LittleEndian.Uint16(f[2:])
			sampleRate := binary.LittleEndian.Uint32(f[4:])
			rate = binary.LittleEndian.Uint32(f[8:])
			align = binary.LittleEndian.Uint16(f[12:])
			bits := binary.LittleEndian.Uint16(f[14:])
			if binary.LittleEndian.Uint16(f[:]) != 1 || n == 18 && binary.LittleEndian.Uint16(f[16:]) != 0 || int(channels) != m.Channels || int(sampleRate) != m.SampleRate || bits != 8 && bits != 16 && bits != 24 && bits != 32 || align != channels*(bits/8) || rate != sampleRate*uint32(align) {
				return errRuntimeInvalid
			}
			haveFmt = true
		case "data":
			if !haveFmt || haveData || n == 0 || align == 0 || n%int64(align) != 0 {
				return errRuntimeInvalid
			}
			haveData = true
			data = n
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return errRuntimeInvalid
			}
		case "JUNK", "LIST", "bext":
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return errRuntimeInvalid
			}
		default:
			return errRuntimeInvalid
		}
		pos += n
		if n&1 != 0 {
			var pad [1]byte
			if pos >= size {
				return errRuntimeInvalid
			}
			if _, e := io.ReadFull(r, pad[:]); e != nil || pad[0] != 0 {
				return errRuntimeInvalid
			}
			pos++
		}
	}
	if !haveFmt || !haveData || rate == 0 || math.Abs(float64(data)/float64(rate)*1000-float64(m.DurationMS)) > 1 {
		return errRuntimeInvalid
	}
	return nil
}
