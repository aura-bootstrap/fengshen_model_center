package scoring

import (
	"context"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

// CandidateInput is the complete structured scoring boundary. Local media and
// ContactSheetPath are never serialized into service snapshots or API payloads.
type CandidateInput struct {
	RequestID            string                         `json:"request_id,omitempty"`
	TaskID               string                         `json:"task_id,omitempty"`
	AnalysisChain        domain.AnalysisChain           `json:"analysis_chain,omitempty"`
	AudioFeatures        *domain.CandidateAudioFeatures `json:"audio_features,omitempty"`
	EpisodeNumber        int                            `json:"episode"`
	CandidateIndex       int                            `json:"candidate_index"`
	StartSec             float64                        `json:"candidate_start_sec"`
	EndSec               float64                        `json:"candidate_end_sec"`
	SourceSignals        []string                       `json:"source_signals,omitempty"`
	EligibleUnits        []domain.SpeechUnit            `json:"eligible_units,omitempty"`
	ContextUnits         []domain.SpeechUnit            `json:"context_units,omitempty"`
	Transcript           string                         `json:"-"`
	Context              string                         `json:"-"`
	AudioSummary         string                         `json:"audio_summary,omitempty"`
	ShotSummary          string                         `json:"shot_summary,omitempty"`
	ContactSheetPath     string                         `json:"-"`
	Image                *ImageInput                    `json:"image,omitempty"`
	MinHighlightScore    float64                        `json:"min_highlight_score"`
	MinCompletenessScore float64                        `json:"min_completeness_score"`
}

// ScoringRequest is retained as the MyClip-compatible name.
type ScoringRequest = CandidateInput

// ValidateAnalysisInput keeps the legacy contact-sheet protocol disjoint from
// audio_text_v1. An empty chain remains legacy for old callers, but cannot be
// combined with new audio features.
func (input CandidateInput) ValidateAnalysisInput() error {
	if input.AnalysisChain == "" {
		if input.AudioFeatures != nil {
			return newError(ErrorKindAnalysisChain, 0, "", "audio features require an explicit analysis chain", nil)
		}
		return nil
	}
	chain, err := domain.ParseAnalysisChain(string(input.AnalysisChain))
	if err != nil {
		return newError(ErrorKindAnalysisChain, 0, "", "unknown highlight analysis chain", nil)
	}
	switch chain {
	case domain.AnalysisChainLegacyContactSheetV1:
		if input.AudioFeatures != nil {
			return newError(ErrorKindAnalysisChain, 0, "", "legacy scoring cannot include audio features", nil)
		}
	case domain.AnalysisChainAudioTextV1, domain.AnalysisChainAudioTextStreamV1:
		if input.AudioFeatures == nil {
			return newError(ErrorKindAudioFeatures, 0, "", "audio-text scoring requires audio features", nil)
		}
		if input.AudioFeatures.Schema != domain.AudioFeatureSchemaV1 {
			return newError(ErrorKindAudioFeatures, 0, "", "unsupported audio feature schema", nil)
		}
		if input.Image != nil || input.ContactSheetPath != "" || input.AudioSummary != "" || input.ShotSummary != "" {
			return newError(ErrorKindAnalysisChain, 0, "", "audio-text scoring cannot include legacy visual or summary fields", nil)
		}
	}
	return nil
}

type ImageInput struct {
	MIMEType   string `json:"mime_type"`
	DataBase64 string `json:"data_base64"`
}

// Usage contains only safe token counters; no prompt, response or image data.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	Attempts         int `json:"attempts"`
	ImageBytes       int `json:"image_bytes,omitempty"`
}

type Result struct {
	RequestID string      `json:"request_id,omitempty"`
	Events    []*RawEvent `json:"events"`
	Usage     Usage       `json:"usage"`
}

// Scorer is shared by the local pipeline, remote adapter and development service.
type Scorer interface {
	ScoreEvents(context.Context, CandidateInput) ([]*RawEvent, error)
}
