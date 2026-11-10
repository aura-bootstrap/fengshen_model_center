package scoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ErrorKind classifies failures without exposing provider response bodies.
type ErrorKind string

const (
	ErrorKindConfig             ErrorKind = "config"
	ErrorKindRequest            ErrorKind = "request"
	ErrorKindTransport          ErrorKind = "transport"
	ErrorKindTimeout            ErrorKind = "timeout"
	ErrorKindCanceled           ErrorKind = "canceled"
	ErrorKindHTTP               ErrorKind = "http"
	ErrorKindProtocol           ErrorKind = "protocol"
	ErrorKindJSON               ErrorKind = "json"
	ErrorKindUnauthorized       ErrorKind = "unauthorized"
	ErrorKindQuota              ErrorKind = "quota"
	ErrorKindAudioAnalysis      ErrorKind = "audio_analysis"
	ErrorKindTranscript         ErrorKind = "transcript"
	ErrorKindLocalModelConfig   ErrorKind = "local_model_config"
	ErrorKindLocalEndpoint      ErrorKind = "local_endpoint"
	ErrorKindLocalSecret        ErrorKind = "local_secret"
	ErrorKindLocalModel         ErrorKind = "local_model"
	ErrorKindAudioFeatures      ErrorKind = "audio_features"
	ErrorKindAnalysisChain      ErrorKind = "analysis_chain"
	ErrorKindAmbiguousExecution ErrorKind = "ambiguous_execution"
	ErrorKindLocalVision        ErrorKind = "local_vision"
)

const (
	CodeConfigInvalid           = "HIGHLIGHT_CONFIG_INVALID"
	CodeRequestInvalid          = "HIGHLIGHT_REQUEST_INVALID"
	CodeUnauthorized            = "HIGHLIGHT_UNAUTHORIZED"
	CodeQuotaExceeded           = "HIGHLIGHT_QUOTA_EXCEEDED"
	CodeProviderTimeout         = "HIGHLIGHT_PROVIDER_TIMEOUT"
	CodeProviderUnavailable     = "HIGHLIGHT_PROVIDER_UNAVAILABLE"
	CodeResponseInvalid         = "HIGHLIGHT_RESPONSE_INVALID"
	CodeCanceled                = "HIGHLIGHT_CANCELED"
	CodeAudioAnalysisFailed     = "HIGHLIGHT_AUDIO_ANALYSIS_FAILED"
	CodeTranscriptFailed        = "HIGHLIGHT_TRANSCRIPT_FAILED"
	CodeLocalModelConfigInvalid = "HIGHLIGHT_LOCAL_MODEL_CONFIG_INVALID"
	CodeLocalEndpointRejected   = "HIGHLIGHT_LOCAL_ENDPOINT_REJECTED"
	CodeLocalSecretUnavailable  = "HIGHLIGHT_LOCAL_SECRET_UNAVAILABLE"
	CodeLocalModelUnavailable   = "HIGHLIGHT_LOCAL_MODEL_UNAVAILABLE"
	CodeAudioFeaturesInvalid    = "HIGHLIGHT_AUDIO_FEATURES_INVALID"
	CodeAnalysisChainInvalid    = "HIGHLIGHT_ANALYSIS_CHAIN_INVALID"
	CodeAmbiguousExecution      = "HIGHLIGHT_AMBIGUOUS_EXECUTION"
	CodeLocalVisionFailed       = "HIGHLIGHT_LOCAL_VISION_FAILED"
)

// ParseCategory describes observable syntax only, not the provider's root cause.
type ParseCategory string

const (
	ParseEmpty         ParseCategory = "empty"
	ParseInvalidJSON   ParseCategory = "invalid_json"
	ParseUnexpectedEOF ParseCategory = "unexpected_eof"
	ParseTrailingJSON  ParseCategory = "trailing_json"
	ParseNonObject     ParseCategory = "non_object"
)

// FinishReason contains only known Chat Completions finish reasons.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishFunctionCall  FinishReason = "function_call"
)

type EventCategory string

const (
	EventObject           EventCategory = "object"
	EventDecode           EventCategory = "decode"
	EventScore            EventCategory = "score"
	EventTitle            EventCategory = "title"
	EventReason           EventCategory = "reason"
	EventTags             EventCategory = "tags"
	EventBool             EventCategory = "bool"
	EventRecommendedPair  EventCategory = "recommended_pair"
	EventRecommendedRange EventCategory = "recommended_range"
)

type ScoreField string

const (
	ScoreFieldHighlight    ScoreField = "highlight_score"
	ScoreFieldConflict     ScoreField = "conflict_score"
	ScoreFieldEmotion      ScoreField = "emotion_score"
	ScoreFieldReversal     ScoreField = "reversal_score"
	ScoreFieldVisual       ScoreField = "visual_score"
	ScoreFieldHook         ScoreField = "hook_score"
	ScoreFieldCompleteness ScoreField = "completeness_score"
)

type ScoreReason string

const (
	ScoreReasonMissing    ScoreReason = "missing"
	ScoreReasonNull       ScoreReason = "null"
	ScoreReasonOutOfRange ScoreReason = "out_of_range"
)

type parseDiagnostic struct {
	category               ParseCategory
	finishReason           FinishReason
	contentLength          int // UTF-8 bytes before trimming; zero also means unavailable.
	eventCategory          EventCategory
	eventCount             int
	invalidEventCount      int
	firstInvalidEventIndex int
	scoreField             ScoreField
	scoreReason            ScoreReason
}

func (e *Error) EventCategory() EventCategory {
	if e == nil {
		return ""
	}
	return e.parse.eventCategory
}

func (e *Error) EventCount() int {
	if e == nil {
		return 0
	}
	return e.parse.eventCount
}

func (e *Error) InvalidEventCount() int {
	if e == nil {
		return 0
	}
	return e.parse.invalidEventCount
}

func (e *Error) FirstInvalidEventIndex() int {
	if e == nil {
		return 0
	}
	return e.parse.firstInvalidEventIndex
}

func (e *Error) ScoreField() ScoreField {
	if e == nil {
		return ""
	}
	return e.parse.scoreField
}

func (e *Error) ScoreReason() ScoreReason {
	if e == nil {
		return ""
	}
	return e.parse.scoreReason
}

func allowedFinishReason(raw json.RawMessage) FinishReason {
	var reason string
	if json.Unmarshal(raw, &reason) != nil {
		return ""
	}
	switch FinishReason(reason) {
	case FinishStop, FinishLength, FinishContentFilter, FinishToolCalls, FinishFunctionCall:
		return FinishReason(reason)
	default:
		return ""
	}
}

// ParseCategory returns local-only syntax metadata; empty means unclassified.
func (e *Error) ParseCategory() ParseCategory {
	if e == nil {
		return ""
	}
	return e.parse.category
}

func (e *Error) FinishReason() FinishReason {
	if e == nil {
		return ""
	}
	return e.parse.finishReason
}

// ContentLength returns the original content length in UTF-8 bytes, if available.
func (e *Error) ContentLength() int {
	if e == nil {
		return 0
	}
	return e.parse.contentLength
}

// Error is safe to return across the server/engine boundary. Message and Code
// are stable; cause is retained only for local errors.Is cancellation checks.
type Error struct {
	Kind       ErrorKind `json:"kind"`
	Code       string    `json:"code"`
	StatusCode int       `json:"status_code,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
	message    string
	cause      error
	parse      parseDiagnostic
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := e.message
	if message == "" {
		message = "highlight scoring failed"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s (%s, status %d): %s", e.Code, e.Kind, e.StatusCode, message)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Kind, message)
}

func (e *Error) Unwrap() error { return e.cause }

// Message returns the sanitized diagnostic without implementation metadata.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

func newError(kind ErrorKind, status int, apiKey, message string, cause error) *Error {
	return &Error{Kind: kind, Code: codeFor(kind), StatusCode: status, message: sanitizeText(message, apiKey), cause: cause}
}

// NewStableError creates a safe authorization/quota/configuration boundary error.
func NewStableError(kind ErrorKind, message string) *Error {
	return newError(kind, 0, "", message, nil)
}

func codeFor(kind ErrorKind) string {
	switch kind {
	case ErrorKindConfig:
		return CodeConfigInvalid
	case ErrorKindRequest:
		return CodeRequestInvalid
	case ErrorKindUnauthorized:
		return CodeUnauthorized
	case ErrorKindQuota:
		return CodeQuotaExceeded
	case ErrorKindTimeout:
		return CodeProviderTimeout
	case ErrorKindCanceled:
		return CodeCanceled
	case ErrorKindProtocol, ErrorKindJSON:
		return CodeResponseInvalid
	case ErrorKindAudioAnalysis:
		return CodeAudioAnalysisFailed
	case ErrorKindTranscript:
		return CodeTranscriptFailed
	case ErrorKindLocalModelConfig:
		return CodeLocalModelConfigInvalid
	case ErrorKindLocalEndpoint:
		return CodeLocalEndpointRejected
	case ErrorKindLocalSecret:
		return CodeLocalSecretUnavailable
	case ErrorKindLocalModel:
		return CodeLocalModelUnavailable
	case ErrorKindAudioFeatures:
		return CodeAudioFeaturesInvalid
	case ErrorKindAnalysisChain:
		return CodeAnalysisChainInvalid
	case ErrorKindAmbiguousExecution:
		return CodeAmbiguousExecution
	case ErrorKindLocalVision:
		return CodeLocalVisionFailed
	default:
		return CodeProviderUnavailable
	}
}

// SanitizeError removes credentials and does not wrap the original error.
func SanitizeError(err error, apiKey string) error {
	if err == nil {
		return nil
	}
	return errors.New(sanitizeText(err.Error(), apiKey))
}

var base64PayloadPattern = regexp.MustCompile(`data:[^;]+;base64,[A-Za-z0-9+/=_-]+`)
var credentialTextPattern = regexp.MustCompile(`(?i)((?:authorization|api[_-]?key|access[_-]?token|secret|token)\s*[:=]\s*(?:bearer\s+)?)[^\s,;]+`)

func sanitizeText(text, apiKey string) string {
	text = credentialTextPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	if apiKey != "" {
		text = strings.ReplaceAll(text, apiKey, "[REDACTED]")
		if encoded := url.QueryEscape(apiKey); encoded != apiKey {
			text = strings.ReplaceAll(text, encoded, "[REDACTED]")
		}
	}
	text = base64PayloadPattern.ReplaceAllString(text, "[IMAGE_REDACTED]")
	return text
}
