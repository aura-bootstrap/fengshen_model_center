package scoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// RawEvent is one model-selected event. Nil entries in ScoreEvents results mark
// invalid events while preserving their original positions.
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

type rawEventWire struct {
	HighlightScore       *float64  `json:"highlight_score"`
	ConflictScore        *float64  `json:"conflict_score"`
	EmotionScore         *float64  `json:"emotion_score"`
	ReversalScore        *float64  `json:"reversal_score"`
	VisualScore          *float64  `json:"visual_score"`
	HookScore            *float64  `json:"hook_score"`
	CompletenessScore    *float64  `json:"completeness_score"`
	StartUnitID          *string   `json:"start_unit_id"`
	EndUnitID            *string   `json:"end_unit_id"`
	RecommendedStartSec  *float64  `json:"recommended_start_sec"`
	RecommendedEndSec    *float64  `json:"recommended_end_sec"`
	Title                *string   `json:"title"`
	Reason               *string   `json:"reason"`
	Tags                 *[]string `json:"tags"`
	IncludeInCompilation *bool     `json:"include_in_compilation"`
}

// classifyContent does not retain decoder errors or any part of the input.
// A second value or invalid bytes after a complete value are trailing JSON.
func classifyContent(data []byte) ParseCategory {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ParseEmpty
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return ParseUnexpectedEOF
		}
		return ParseInvalidJSON
	}
	if len(bytes.TrimSpace(data[decoder.InputOffset():])) != 0 {
		return ParseTrailingJSON
	}
	if value[0] != '{' {
		return ParseNonObject
	}
	return ""
}

func parseEvents(content string) ([]*RawEvent, error) {
	return parseEventsDiagnostic(content, nil)
}

func parseEventsDiagnostic(content string, diagnostic *parseDiagnostic) (result []*RawEvent, resultErr error) {
	if diagnostic != nil {
		diagnostic.eventCategory = ""
		diagnostic.eventCount = 0
		diagnostic.invalidEventCount = 0
		diagnostic.firstInvalidEventIndex = 0
		diagnostic.scoreField = ""
		diagnostic.scoreReason = ""
	}
	defer func() {
		if classified, ok := resultErr.(*Error); ok {
			classified.cause = nil
			classified.parse.category = classifyContent([]byte(content))
			classified.parse.contentLength = len(content)
		}
	}()
	contentBytes := bytes.TrimSpace([]byte(content))
	if len(contentBytes) > 0 && contentBytes[0] != '{' {
		var value any
		if err := decodeStrictJSON(contentBytes, &value); err != nil {
			return nil, newError(ErrorKindJSON, 0, "", "model content is not strict JSON", err)
		}
		return nil, newError(ErrorKindProtocol, 0, "", "model content must be a JSON object", nil)
	}
	var object map[string]json.RawMessage
	if err := decodeStrictJSON(contentBytes, &object); err != nil {
		return nil, newError(ErrorKindJSON, 0, "", "model content is not strict JSON", err)
	}
	if object == nil {
		return nil, newError(ErrorKindProtocol, 0, "", "model content must be a JSON object", nil)
	}

	var rawEvents []json.RawMessage
	if eventsJSON, ok := object["events"]; ok {
		trimmed := bytes.TrimSpace(eventsJSON)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, newError(ErrorKindProtocol, 0, "", "events must be an array", nil)
		}
		if err := decodeStrictJSON(eventsJSON, &rawEvents); err != nil {
			return nil, newError(ErrorKindProtocol, 0, "", "events must be an array", err)
		}
	} else if _, ok := object["highlight_score"]; ok {
		legacy, err := json.Marshal(object)
		if err != nil {
			return nil, newError(ErrorKindProtocol, 0, "", "invalid legacy event", err)
		}
		rawEvents = []json.RawMessage{legacy}
	} else {
		return nil, newError(ErrorKindProtocol, 0, "", "model content has no events", nil)
	}

	events := make([]*RawEvent, len(rawEvents))
	if diagnostic != nil {
		diagnostic.eventCount = len(rawEvents)
	}
	for index, raw := range rawEvents {
		var eventDiagnostic parseDiagnostic
		event, category := parseRawEventWithDiagnostic(raw, &eventDiagnostic)
		events[index] = event
		if category != "" && diagnostic != nil {
			if diagnostic.invalidEventCount == 0 {
				diagnostic.eventCategory = category
				diagnostic.firstInvalidEventIndex = index + 1
				diagnostic.scoreField = eventDiagnostic.scoreField
				diagnostic.scoreReason = eventDiagnostic.scoreReason
			}
			diagnostic.invalidEventCount++
		}
	}
	return events, nil
}

func parseRawEvent(raw json.RawMessage) (*RawEvent, bool) {
	event, category := parseRawEventDiagnostic(raw)
	return event, category == ""
}

func parseRawEventDiagnostic(raw json.RawMessage) (*RawEvent, EventCategory) {
	return parseRawEventWithDiagnostic(raw, nil)
}

func parseRawEventWithDiagnostic(raw json.RawMessage, diagnostic *parseDiagnostic) (*RawEvent, EventCategory) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, EventObject
	}
	var wire rawEventWire
	if err := decodeStrictJSON(trimmed, &wire); err != nil {
		return nil, EventDecode
	}
	scores := []struct {
		field ScoreField
		value *float64
	}{
		{ScoreFieldHighlight, wire.HighlightScore},
		{ScoreFieldConflict, wire.ConflictScore},
		{ScoreFieldEmotion, wire.EmotionScore},
		{ScoreFieldReversal, wire.ReversalScore},
		{ScoreFieldVisual, wire.VisualScore},
		{ScoreFieldHook, wire.HookScore},
		{ScoreFieldCompleteness, wire.CompletenessScore},
	}
	for _, score := range scores {
		if score.value == nil || !validScore(*score.value) {
			if diagnostic != nil {
				diagnostic.scoreField = score.field
				diagnostic.scoreReason = ScoreReasonOutOfRange
				if score.value == nil {
					var fields map[string]json.RawMessage
					if json.Unmarshal(trimmed, &fields) != nil {
						diagnostic.scoreField, diagnostic.scoreReason = "", ""
						return nil, EventDecode
					}
					diagnostic.scoreReason = ScoreReasonMissing
					if _, exists := fields[string(score.field)]; exists {
						diagnostic.scoreReason = ScoreReasonNull
					}
				}
			}
			return nil, EventScore
		}
	}
	if wire.Title == nil || strings.TrimSpace(*wire.Title) == "" {
		return nil, EventTitle
	}
	if wire.Reason == nil || strings.TrimSpace(*wire.Reason) == "" {
		return nil, EventReason
	}
	if wire.Tags == nil {
		return nil, EventTags
	}
	for _, tag := range *wire.Tags {
		if strings.TrimSpace(tag) == "" {
			return nil, EventTags
		}
	}
	if wire.IncludeInCompilation == nil {
		return nil, EventBool
	}
	if (wire.RecommendedStartSec == nil) != (wire.RecommendedEndSec == nil) {
		return nil, EventRecommendedPair
	}
	if wire.RecommendedStartSec != nil {
		start, end := *wire.RecommendedStartSec, *wire.RecommendedEndSec
		if !finite(start) || !finite(end) || start < 0 || end <= 0 || end <= start {
			return nil, EventRecommendedRange
		}
	}

	return &RawEvent{
		HighlightScore:       *wire.HighlightScore,
		ConflictScore:        *wire.ConflictScore,
		EmotionScore:         *wire.EmotionScore,
		ReversalScore:        *wire.ReversalScore,
		VisualScore:          *wire.VisualScore,
		HookScore:            *wire.HookScore,
		CompletenessScore:    *wire.CompletenessScore,
		StartUnitID:          wire.StartUnitID,
		EndUnitID:            wire.EndUnitID,
		RecommendedStartSec:  wire.RecommendedStartSec,
		RecommendedEndSec:    wire.RecommendedEndSec,
		Title:                *wire.Title,
		Reason:               *wire.Reason,
		Tags:                 *wire.Tags,
		IncludeInCompilation: *wire.IncludeInCompilation,
	}, ""
}

// ValidateEvent applies the same per-event schema rules to provider responses
// and injected scorers used by local development.
func ValidateEvent(event *RawEvent) error {
	if event == nil {
		return fmt.Errorf("事件结构为空")
	}
	for _, score := range []float64{event.HighlightScore, event.ConflictScore, event.EmotionScore, event.ReversalScore, event.VisualScore, event.HookScore, event.CompletenessScore} {
		if !validScore(score) {
			return fmt.Errorf("事件评分无效")
		}
	}
	if strings.TrimSpace(event.Title) == "" || strings.TrimSpace(event.Reason) == "" || event.Tags == nil {
		return fmt.Errorf("事件标题、理由或标签无效")
	}
	for _, tag := range event.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("事件标签无效")
		}
	}
	if (event.RecommendedStartSec == nil) != (event.RecommendedEndSec == nil) {
		return fmt.Errorf("推荐起止秒数必须成对出现")
	}
	if event.RecommendedStartSec != nil && (!finite(*event.RecommendedStartSec) || !finite(*event.RecommendedEndSec) || *event.RecommendedStartSec < 0 || *event.RecommendedEndSec <= *event.RecommendedStartSec) {
		return fmt.Errorf("推荐起止秒数无效")
	}
	return nil
}

func validScore(value float64) bool {
	return finite(value) && value >= 0 && value <= 1
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
