package domain

import (
	"errors"
	"fmt"
)

const (
	AudioFeatureSchemaV1 = "audio_features_v1"
	AudioTextPromptV1    = "audio_text_prompt_v1"
)

var ErrInvalidAnalysisVersions = errors.New("invalid highlight analysis version combination")

// ValidateAnalysisVersions rejects partial or mixed protocol identities. Legacy
// snapshots have no audio feature schema or audio-text prompt version.
func ValidateAnalysisVersions(chain AnalysisChain, audioFeatureSchema, promptVersion string) error {
	parsed, err := ParseAnalysisChain(string(chain))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAnalysisVersions, err)
	}
	switch parsed {
	case AnalysisChainAudioTextV1, AnalysisChainAudioTextStreamV1:
		if audioFeatureSchema != AudioFeatureSchemaV1 || promptVersion != AudioTextPromptV1 {
			return fmt.Errorf("%w: %s requires audio_features_v1 and audio_text_prompt_v1", ErrInvalidAnalysisVersions, parsed)
		}
	case AnalysisChainLegacyContactSheetV1:
		if audioFeatureSchema != "" || promptVersion != "" {
			return fmt.Errorf("%w: legacy execution cannot carry audio-text versions", ErrInvalidAnalysisVersions)
		}
	}
	return nil
}
