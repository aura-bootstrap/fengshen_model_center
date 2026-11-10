package scoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const audioTextSystemPrompt = "你是短剧音频文本高光审核器。仅依据台词、语句时间和结构化音频特征审核，只返回严格 JSON，不要包含 Markdown、解释或额外文本。"

// ModelConfig is the public model configuration accepted by the shared text
// request constructor. Only Model is used; credentials and endpoints never
// enter TextRequest.
type ModelConfig = Config

// TextRequest is the text-only OpenAI Chat Completions wire request. String
// message content makes multimodal content impossible on this path.
type TextRequest struct {
	Model          string             `json:"model"`
	Messages       []TextMessage      `json:"messages"`
	Temperature    float64            `json:"temperature"`
	N              int                `json:"n"`
	ResponseFormat TextResponseFormat `json:"response_format"`
}

type TextMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type TextResponseFormat struct {
	Type string `json:"type"`
}

// ValidationWindow binds one candidate and its read-only context to the
// feature projection. The feature window may be wider than the candidate, but
// must contain it and every supplied unit.
type ValidationWindow struct {
	StartSec         float64
	EndSec           float64
	MediaDurationSec float64
	EligibleUnits    []domain.SpeechUnit
	ContextUnits     []domain.SpeechUnit
}

func ValidateAudioFeatures(features domain.CandidateAudioFeatures, window ValidationWindow) error {
	if !domain.ValidRange(window.StartSec, window.EndSec) ||
		(window.MediaDurationSec != 0 && (!domain.Finite(window.MediaDurationSec) || window.MediaDurationSec <= 0 || window.EndSec > window.MediaDurationSec)) {
		return newError(ErrorKindAudioFeatures, 0, "", "audio feature validation window is invalid", nil)
	}
	if features.Window.StartSec > window.StartSec || features.Window.EndSec < window.EndSec {
		return newError(ErrorKindAudioFeatures, 0, "", "audio feature window does not contain candidate", nil)
	}
	mediaDuration := window.MediaDurationSec
	if mediaDuration == 0 {
		mediaDuration = features.Window.EndSec
	}
	if err := domain.ValidateAudioFeatures(features, domain.AudioFeatureValidationWindow{
		StartSec: features.Window.StartSec, EndSec: features.Window.EndSec, MediaDurationSec: mediaDuration,
		EligibleUnits: window.EligibleUnits, ContextUnits: window.ContextUnits,
	}); err != nil {
		return newError(ErrorKindAudioFeatures, 0, "", "audio features are invalid", nil)
	}
	return nil
}

type textCandidateWire struct {
	PromptVersion        string                         `json:"prompt_version"`
	AnalysisChain        domain.AnalysisChain           `json:"analysis_chain"`
	RequestID            string                         `json:"request_id"`
	TaskID               string                         `json:"task_id,omitempty"`
	EpisodeNumber        int                            `json:"episode"`
	CandidateIndex       int                            `json:"candidate_index"`
	StartSec             float64                        `json:"candidate_start_sec"`
	EndSec               float64                        `json:"candidate_end_sec"`
	SourceSignals        []string                       `json:"source_signals,omitempty"`
	EligibleUnits        []domain.SpeechUnit            `json:"eligible_units"`
	ContextUnits         []domain.SpeechUnit            `json:"context_units"`
	AudioFeatures        *domain.CandidateAudioFeatures `json:"audio_features"`
	MinHighlightScore    float64                        `json:"min_highlight_score"`
	MinCompletenessScore float64                        `json:"min_completeness_score"`
}

// SerializeTextCandidateInput returns the canonical structured candidate JSON
// embedded in every text-scoring route. Local transcript/context strings and
// all legacy media fields are intentionally outside this projection.
func SerializeTextCandidateInput(input CandidateInput) ([]byte, error) {
	normalized, err := normalizeTextCandidateInput(input)
	if err != nil {
		return nil, err
	}
	return marshalTextCandidate(normalized)
}

func marshalTextCandidate(input CandidateInput) ([]byte, error) {
	return json.Marshal(textCandidateWire{
		PromptVersion: domain.AudioTextPromptV1, AnalysisChain: input.AnalysisChain,
		RequestID: input.RequestID, TaskID: input.TaskID, EpisodeNumber: input.EpisodeNumber,
		CandidateIndex: input.CandidateIndex, StartSec: input.StartSec, EndSec: input.EndSec,
		SourceSignals: input.SourceSignals, EligibleUnits: input.EligibleUnits, ContextUnits: input.ContextUnits,
		AudioFeatures: input.AudioFeatures, MinHighlightScore: input.MinHighlightScore,
		MinCompletenessScore: input.MinCompletenessScore,
	})
}

// BuildAudioTextPrompt constructs only audio_text_prompt_v1. It has no legacy
// image summary, sampled-frame or arbitrary-second output instructions.
func BuildAudioTextPrompt(input CandidateInput) (string, error) {
	normalized, err := normalizeTextCandidateInput(input)
	if err != nil {
		return "", err
	}
	candidateJSON, err := marshalTextCandidate(normalized)
	if err != nil {
		return "", newError(ErrorKindRequest, 0, "", "cannot encode audio-text candidate", nil)
	}
	return "协议版本：" + domain.AudioTextPromptV1 + "\n" +
		"目标：根据候选台词、连续语句、前后文和结构化音频特征，找出剧情冲突、反转、笑点、情绪爆发及其他有吸引力的完整事件。\n" +
		"同时判断语句与事件完整性，保留理解吸引点所需的铺垫和回应；纯过渡、重复表达或没有吸引点的内容应排除。\n" +
		"音量、语速、音高、停顿和重叠区间只是声音变化证据，不得伪造未提供的测量、身份或情绪结论。\n" +
		"高光门槛为 highlight_score >= " + formatScore(normalized.MinHighlightScore) +
		"，完整性门槛为 completeness_score >= " + formatScore(normalized.MinCompletenessScore) + "。\n" +
		"只返回 JSON 对象 {\"events\":[...]}；没有合格事件时返回 {\"events\":[]}。同一候选可返回多个不同事件，不得拼接不连续事件。\n" +
		"每个事件只能包含 highlight_score、conflict_score、emotion_score、reversal_score、hook_score、completeness_score、" +
		"start_unit_id、end_unit_id、title、reason、tags、include_in_compilation，且这些字段全部必填；所有分数位于 0 到 1。\n" +
		"start_unit_id 和 end_unit_id 必须从 eligible_units 选择，范围包含两端且中间语句连续；不得选择 context_units 独有 ID，不得跳句、反序或编造 ID。\n" +
		"只能选择 alignment_reliable=true 的语句范围。除输入已有时间外，不得输出任何秒数或时间范围字段。\n" +
		"title 和 reason 必须非空，tags 必须是字符串数组，include_in_compilation 必须是布尔值；不得增加字段。\n" +
		"候选输入（严格 JSON）：\n" + string(candidateJSON), nil
}

// BuildTextRequest constructs the one shared single-choice, strict-JSON text
// request used by loopback, direct and Highlight API adapters.
func BuildTextRequest(input CandidateInput, config ModelConfig) (TextRequest, error) {
	if strings.TrimSpace(config.Model) == "" {
		return TextRequest{}, newError(ErrorKindRequest, 0, "", "text scoring model is required", nil)
	}
	prompt, err := BuildAudioTextPrompt(input)
	if err != nil {
		return TextRequest{}, err
	}
	return TextRequest{
		Model: strings.TrimSpace(config.Model),
		Messages: []TextMessage{
			{Role: "system", Content: audioTextSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2, N: 1, ResponseFormat: TextResponseFormat{Type: "json_object"},
	}, nil
}

func normalizeTextCandidateInput(input CandidateInput) (CandidateInput, error) {
	normalized, err := normalizeScoringRequest(input)
	if err != nil {
		return CandidateInput{}, err
	}
	if !domain.IsAudioTextAnalysis(normalized.AnalysisChain) {
		return CandidateInput{}, newError(ErrorKindAnalysisChain, 0, "", "text scoring requires audio_text_v1", nil)
	}
	if normalized.AnalysisChain == domain.AnalysisChainAudioTextV1 &&
		(normalized.MinHighlightScore < DefaultMinHighlightScore || normalized.MinCompletenessScore < DefaultMinCompletenessScore) {
		return CandidateInput{}, newError(ErrorKindRequest, 0, "", "audio-text scoring thresholds cannot be lowered", nil)
	}
	if len(normalized.EligibleUnits) == 0 {
		return CandidateInput{}, newError(ErrorKindRequest, 0, "", "audio-text scoring requires eligible units", nil)
	}
	seen := make(map[string]struct{}, len(normalized.EligibleUnits))
	previousEnd := -1.0
	for _, unit := range normalized.EligibleUnits {
		if strings.TrimSpace(unit.ID) == "" || !domain.ValidRange(unit.StartSec, unit.EndSec) || strings.TrimSpace(unit.Text) == "" || unit.StartSec < previousEnd {
			return CandidateInput{}, newError(ErrorKindRequest, 0, "", "eligible units are invalid or unstable", nil)
		}
		if _, exists := seen[unit.ID]; exists {
			return CandidateInput{}, newError(ErrorKindRequest, 0, "", "eligible unit IDs must be unique", nil)
		}
		seen[unit.ID] = struct{}{}
		previousEnd = unit.EndSec
	}
	if err := ValidateAudioFeatures(*normalized.AudioFeatures, ValidationWindow{
		StartSec: normalized.StartSec, EndSec: normalized.EndSec,
		EligibleUnits: normalized.EligibleUnits, ContextUnits: normalized.ContextUnits,
	}); err != nil {
		return CandidateInput{}, err
	}
	return normalized, nil
}

type textResponseEnvelope struct {
	ID                json.RawMessage `json:"id,omitempty"`
	Object            json.RawMessage `json:"object,omitempty"`
	Created           json.RawMessage `json:"created,omitempty"`
	Model             json.RawMessage `json:"model,omitempty"`
	Choices           []textChoice    `json:"choices"`
	Usage             textUsage       `json:"usage,omitempty"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint,omitempty"`
	ServiceTier       json.RawMessage `json:"service_tier,omitempty"`
	Timings           json.RawMessage `json:"timings,omitempty"`
}

type textChoice struct {
	Index                json.RawMessage   `json:"index,omitempty"`
	Message              textChoiceMessage `json:"message"`
	FinishReason         *string           `json:"finish_reason,omitempty"`
	Logprobs             json.RawMessage   `json:"logprobs,omitempty"`
	ContentFilterResults json.RawMessage   `json:"content_filter_results,omitempty"`
}

type textChoiceMessage struct {
	Role             json.RawMessage `json:"role,omitempty"`
	Content          *string         `json:"content"`
	Refusal          json.RawMessage `json:"refusal,omitempty"`
	ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
	Annotations      json.RawMessage `json:"annotations,omitempty"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
}

type textUsage struct {
	PromptTokens            int             `json:"prompt_tokens,omitempty"`
	CompletionTokens        int             `json:"completion_tokens,omitempty"`
	TotalTokens             int             `json:"total_tokens,omitempty"`
	PromptTokensDetails     json.RawMessage `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails json.RawMessage `json:"completion_tokens_details,omitempty"`
}

type textContentEnvelope struct {
	Events json.RawMessage `json:"events"`
}

type textEventWire struct {
	HighlightScore       *float64  `json:"highlight_score"`
	ConflictScore        *float64  `json:"conflict_score"`
	EmotionScore         *float64  `json:"emotion_score"`
	ReversalScore        *float64  `json:"reversal_score"`
	HookScore            *float64  `json:"hook_score"`
	CompletenessScore    *float64  `json:"completeness_score"`
	StartUnitID          *string   `json:"start_unit_id"`
	EndUnitID            *string   `json:"end_unit_id"`
	Title                *string   `json:"title"`
	Reason               *string   `json:"reason"`
	Tags                 *[]string `json:"tags"`
	IncludeInCompilation *bool     `json:"include_in_compilation"`
}

// ParseTextResponse strictly parses one OpenAI choice and its complete
// multi-event payload. Any malformed event rejects the whole candidate; no
// Markdown repair, truncated JSON recovery or legacy single-event fallback is
// permitted on audio_text_v1.
func ParseTextResponse(data []byte, input CandidateInput) (Result, error) {
	normalized, err := normalizeTextCandidateInput(input)
	if err != nil {
		return Result{}, err
	}
	var response textResponseEnvelope
	if err := decodeStrictTextJSON(data, &response); err != nil {
		return Result{}, newError(ErrorKindProtocol, 0, "", "invalid text Chat Completions response", nil)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Content == nil {
		return Result{}, newError(ErrorKindProtocol, 0, "", "text response must contain exactly one choice with string content", nil)
	}
	choice := response.Choices[0]
	if choice.FinishReason != nil && *choice.FinishReason == "length" {
		return Result{}, newError(ErrorKindProtocol, 0, "", "text model response was truncated", nil)
	}
	content := bytes.TrimSpace([]byte(*choice.Message.Content))
	if len(content) == 0 || content[0] != '{' {
		return Result{}, newError(ErrorKindJSON, 0, "", "text model content is not strict JSON", nil)
	}
	var envelope textContentEnvelope
	if err := decodeStrictTextJSON(content, &envelope); err != nil {
		return Result{}, newError(ErrorKindJSON, 0, "", "text model content is not strict JSON", nil)
	}
	trimmedEvents := bytes.TrimSpace(envelope.Events)
	if len(trimmedEvents) == 0 || trimmedEvents[0] != '[' {
		return Result{}, newError(ErrorKindProtocol, 0, "", "text response events must be an array", nil)
	}
	var rawEvents []json.RawMessage
	if err := decodeStrictTextJSON(trimmedEvents, &rawEvents); err != nil {
		return Result{}, newError(ErrorKindProtocol, 0, "", "text response events must be an array", nil)
	}
	events := make([]*RawEvent, len(rawEvents))
	for index, raw := range rawEvents {
		var diagnostic parseDiagnostic
		event, err := parseTextEventWithDiagnostic(raw, &diagnostic)
		diagnostic.eventCount = len(rawEvents)
		diagnostic.invalidEventCount = 1
		diagnostic.firstInvalidEventIndex = index + 1
		if err != nil {
			classified := newError(ErrorKindProtocol, 0, "", "text response contains an invalid event at index "+strconv.Itoa(index), nil)
			classified.parse = diagnostic
			return Result{}, classified
		}
		if err := validateTextEventUnits(event, normalized); err != nil {
			if classified, ok := err.(*Error); ok {
				classified.parse = diagnostic
			}
			return Result{}, err
		}
		events[index] = event
	}
	return Result{
		RequestID: normalized.RequestID, Events: events,
		Usage: Usage{PromptTokens: response.Usage.PromptTokens, CompletionTokens: response.Usage.CompletionTokens, TotalTokens: response.Usage.TotalTokens},
	}, nil
}

func parseTextEvent(raw json.RawMessage) (*RawEvent, error) {
	return parseTextEventWithDiagnostic(raw, nil)
}

func parseTextEventWithDiagnostic(raw json.RawMessage, diagnostic *parseDiagnostic) (*RawEvent, error) {
	if diagnostic != nil {
		*diagnostic = parseDiagnostic{}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if diagnostic != nil {
			diagnostic.eventCategory = EventObject
		}
		return nil, fmt.Errorf("event must be an object")
	}
	var wire textEventWire
	if err := decodeStrictTextJSON(trimmed, &wire); err != nil {
		if diagnostic != nil {
			diagnostic.eventCategory = EventDecode
		}
		return nil, err
	}
	scores := []struct {
		field ScoreField
		value *float64
	}{
		{ScoreFieldHighlight, wire.HighlightScore},
		{ScoreFieldConflict, wire.ConflictScore},
		{ScoreFieldEmotion, wire.EmotionScore},
		{ScoreFieldReversal, wire.ReversalScore},
		{ScoreFieldHook, wire.HookScore},
		{ScoreFieldCompleteness, wire.CompletenessScore},
	}
	for _, score := range scores {
		if score.value == nil || !validScore(*score.value) {
			if diagnostic != nil {
				diagnostic.eventCategory = EventScore
				diagnostic.scoreField = score.field
				diagnostic.scoreReason = ScoreReasonOutOfRange
				if score.value == nil {
					var fields map[string]json.RawMessage
					_ = json.Unmarshal(trimmed, &fields) // Strict decoding already validated this object.
					diagnostic.scoreReason = ScoreReasonMissing
					if _, exists := fields[string(score.field)]; exists {
						diagnostic.scoreReason = ScoreReasonNull
					}
				}
			}
			return nil, fmt.Errorf("event score is missing or invalid")
		}
	}
	if wire.StartUnitID == nil || strings.TrimSpace(*wire.StartUnitID) == "" || wire.EndUnitID == nil || strings.TrimSpace(*wire.EndUnitID) == "" ||
		wire.Title == nil || strings.TrimSpace(*wire.Title) == "" || wire.Reason == nil || strings.TrimSpace(*wire.Reason) == "" ||
		wire.Tags == nil || wire.IncludeInCompilation == nil {
		if diagnostic != nil {
			switch {
			case wire.StartUnitID == nil || strings.TrimSpace(*wire.StartUnitID) == "" || wire.EndUnitID == nil || strings.TrimSpace(*wire.EndUnitID) == "":
				// Unit validation has no matching event category.
			case wire.Title == nil || strings.TrimSpace(*wire.Title) == "":
				diagnostic.eventCategory = EventTitle
			case wire.Reason == nil || strings.TrimSpace(*wire.Reason) == "":
				diagnostic.eventCategory = EventReason
			case wire.Tags == nil:
				diagnostic.eventCategory = EventTags
			case wire.IncludeInCompilation == nil:
				diagnostic.eventCategory = EventBool
			}
		}
		return nil, fmt.Errorf("event required field is missing")
	}
	for _, tag := range *wire.Tags {
		if strings.TrimSpace(tag) == "" {
			if diagnostic != nil {
				diagnostic.eventCategory = EventTags
			}
			return nil, fmt.Errorf("event tag is invalid")
		}
	}
	return &RawEvent{
		HighlightScore: *wire.HighlightScore, ConflictScore: *wire.ConflictScore,
		EmotionScore: *wire.EmotionScore, ReversalScore: *wire.ReversalScore, HookScore: *wire.HookScore,
		CompletenessScore: *wire.CompletenessScore, StartUnitID: wire.StartUnitID, EndUnitID: wire.EndUnitID,
		Title: *wire.Title, Reason: *wire.Reason, Tags: append([]string(nil), (*wire.Tags)...),
		IncludeInCompilation: *wire.IncludeInCompilation,
	}, nil
}

func validateTextEventUnits(event *RawEvent, input CandidateInput) error {
	eligible := make(map[string]int, len(input.EligibleUnits))
	combined := make(map[string]domain.SpeechUnit, len(input.EligibleUnits)+len(input.ContextUnits))
	for index, unit := range input.EligibleUnits {
		eligible[unit.ID] = index
		combined[unit.ID] = unit
	}
	for _, unit := range input.ContextUnits {
		if _, exists := combined[unit.ID]; !exists {
			combined[unit.ID] = unit
		}
	}
	first, firstOK := eligible[*event.StartUnitID]
	last, lastOK := eligible[*event.EndUnitID]
	if !firstOK || !lastOK || last < first {
		return newError(ErrorKindProtocol, 0, "", "text event unit IDs must be eligible and ordered", nil)
	}
	for _, unit := range input.EligibleUnits[first : last+1] {
		if !unit.AlignmentReliable {
			return newError(ErrorKindProtocol, 0, "", "text event selects unreliable alignment", nil)
		}
	}
	ordered := make([]domain.SpeechUnit, 0, len(combined))
	for _, unit := range combined {
		ordered = append(ordered, unit)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].StartSec == ordered[j].StartSec {
			return ordered[i].EndSec < ordered[j].EndSec
		}
		return ordered[i].StartSec < ordered[j].StartSec
	})
	selecting := false
	seenEnd := false
	for _, unit := range ordered {
		if unit.ID == *event.StartUnitID {
			selecting = true
		}
		if selecting {
			if _, ok := eligible[unit.ID]; !ok {
				return newError(ErrorKindProtocol, 0, "", "text event range skips a non-eligible unit", nil)
			}
		}
		if selecting && unit.ID == *event.EndUnitID {
			seenEnd = true
			break
		}
	}
	if !seenEnd {
		return newError(ErrorKindProtocol, 0, "", "text event unit range is not continuous", nil)
	}
	return nil
}

func decodeStrictTextJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
