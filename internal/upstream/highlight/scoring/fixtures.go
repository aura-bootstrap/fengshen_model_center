package scoring

import (
	"encoding/json"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

// ContractCandidate is the shared text/protocol fixture used by service,
// pipeline and mock contract tests.
func ContractCandidate() CandidateInput {
	return CandidateInput{
		RequestID: "contract-request-001", TaskID: "contract-task", EpisodeNumber: 1, CandidateIndex: 0,
		StartSec: 0, EndSec: 60, SourceSignals: []string{"scene_boundary"},
		EligibleUnits: []domain.SpeechUnit{
			{ID: "U00001", StartSec: 1, EndSec: 5, Text: "第一句。", AlignmentReliable: true},
			{ID: "U00002", StartSec: 5, EndSec: 10, Text: "第二句。", AlignmentReliable: true},
			{ID: "U00003", StartSec: 20, EndSec: 25, Text: "第三句。", AlignmentReliable: true},
			{ID: "U00004", StartSec: 25, EndSec: 30, Text: "第四句。", AlignmentReliable: true},
		},
		AudioSummary: "未测量额外音频特征", ShotSummary: "scene_boundary",
		MinHighlightScore: DefaultMinHighlightScore, MinCompletenessScore: DefaultMinCompletenessScore,
	}
}

// ContractEvents returns fresh deterministic multi-event values on every call.
func ContractEvents() []*RawEvent {
	firstStart, firstEnd, secondStart, secondEnd := "U00001", "U00002", "U00003", "U00004"
	return []*RawEvent{
		{HighlightScore: 0.95, ConflictScore: 0.9, EmotionScore: 0.85, ReversalScore: 0.8, VisualScore: 0.75, HookScore: 0.9, CompletenessScore: 0.95, StartUnitID: &firstStart, EndUnitID: &firstEnd, Title: "完整冲突", Reason: "有铺垫和回应", Tags: []string{"冲突"}, IncludeInCompilation: true},
		{HighlightScore: 0.90, ConflictScore: 0.85, EmotionScore: 0.9, ReversalScore: 0.85, VisualScore: 0.8, HookScore: 0.85, CompletenessScore: 0.9, StartUnitID: &secondStart, EndUnitID: &secondEnd, Title: "情绪反转", Reason: "完整情绪变化", Tags: []string{"反转"}, IncludeInCompilation: true},
	}
}

// ContractContent is the canonical strict multi-event model response.
func ContractContent() (string, error) {
	data, err := json.Marshal(map[string]any{"events": ContractEvents()})
	return string(data), err
}
