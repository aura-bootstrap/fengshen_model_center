package domain

import "errors"

type AnalysisChain string

const (
	AnalysisChainAudioTextV1          AnalysisChain = "audio_text_v1"
	AnalysisChainAudioTextStreamV1    AnalysisChain = "audio_text_stream_v1"
	AnalysisChainLegacyContactSheetV1 AnalysisChain = "legacy_contact_sheet_v1"
)

var ErrInvalidAnalysisChain = errors.New("invalid highlight analysis chain")

func ParseAnalysisChain(value string) (AnalysisChain, error) {
	chain := AnalysisChain(value)
	switch chain {
	case AnalysisChainAudioTextV1, AnalysisChainAudioTextStreamV1, AnalysisChainLegacyContactSheetV1:
		return chain, nil
	default:
		return "", ErrInvalidAnalysisChain
	}
}

func IsAudioTextAnalysis(chain AnalysisChain) bool {
	return chain == AnalysisChainAudioTextV1 || chain == AnalysisChainAudioTextStreamV1
}
