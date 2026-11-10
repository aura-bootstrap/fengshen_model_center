package domain

type EventDecision struct {
	EventIndex        int      `json:"event_index"`
	Outcome           string   `json:"outcome"`
	Reason            string   `json:"reason"`
	Title             *string  `json:"title,omitempty"`
	HighlightScore    *float64 `json:"highlight_score,omitempty"`
	CompletenessScore *float64 `json:"completeness_score,omitempty"`
	StartUnitID       *string  `json:"start_unit_id,omitempty"`
	EndUnitID         *string  `json:"end_unit_id,omitempty"`
}

type CandidateFailureDiagnostic struct {
	Stage                 string  `json:"stage"`
	Code                  string  `json:"code"`
	WindowStartSec        float64 `json:"window_start_sec"`
	WindowEndSec          float64 `json:"window_end_sec"`
	CoverageStartSec      float64 `json:"coverage_start_sec"`
	CoverageEndSec        float64 `json:"coverage_end_sec"`
	EligibleUnits         int     `json:"eligible_units"`
	ContextUnits          int     `json:"context_units"`
	EligibleOutsideWindow int     `json:"eligible_outside_window"`
	ContextOutsideWindow  int     `json:"context_outside_window"`
}

type CandidateEvaluation struct {
	Highlights        []HighlightSegment          `json:"highlights"`
	Decisions         []EventDecision             `json:"decisions"`
	Failure           string                      `json:"failure,omitempty"`
	FailureDiagnostic *CandidateFailureDiagnostic `json:"failure_diagnostic,omitempty"`
}

type CandidateAudit struct {
	EpisodeNumber     int                         `json:"episode_number"`
	CandidateIndex    int                         `json:"candidate_index"`
	StartSec          float64                     `json:"start_sec"`
	EndSec            float64                     `json:"end_sec"`
	Failure           string                      `json:"failure,omitempty"`
	FailureDiagnostic *CandidateFailureDiagnostic `json:"failure_diagnostic,omitempty"`
	Decisions         []EventDecision             `json:"decisions"`
}

type AnalysisSummary struct {
	ExtractionGoal          string           `json:"extraction_goal"`
	MinHighlightScore       float64          `json:"min_highlight_score"`
	MinCompletenessScore    float64          `json:"min_completeness_score"`
	RequestedEpisodes       int              `json:"requested_episodes"`
	ProcessedEpisodes       int              `json:"processed_episodes"`
	EpisodeFailures         int              `json:"episode_failures"`
	UnprocessedEpisodes     int              `json:"unprocessed_episodes"`
	PreparationFailure      string           `json:"preparation_failure,omitempty"`
	Candidates              int              `json:"candidates"`
	ReturnedEvents          int              `json:"returned_events"`
	AcceptedEvents          int              `json:"accepted_events"`
	RejectedEvents          map[string]int   `json:"rejected_events"`
	CandidateFailures       map[string]int   `json:"candidate_failures"`
	CandidatesWithoutEvents int              `json:"candidates_without_events"`
	DuplicateEvents         int              `json:"duplicate_events"`
	LimitedEvents           int              `json:"limited_events"`
	ExportFailedEvents      int              `json:"export_failed_events"`
	SaveFailedEvents        int              `json:"save_failed_events"`
	SavedEvents             int              `json:"saved_events"`
	Target                  int              `json:"target,omitempty"`
	SelectedEvents          int              `json:"selected_events"`
	TargetShortfall         int              `json:"target_shortfall"`
	AnalysisStopReason      string           `json:"analysis_stop_reason,omitempty"`
	ZeroOutput              bool             `json:"zero_output"`
	Warnings                []string         `json:"warnings,omitempty"`
	CandidateAudits         []CandidateAudit `json:"candidate_audits"`
}

func NewAnalysisSummary(requested int, minHighlight, minCompleteness float64) AnalysisSummary {
	return AnalysisSummary{
		ExtractionGoal: "comprehensive", MinHighlightScore: minHighlight,
		MinCompletenessScore: minCompleteness, RequestedEpisodes: requested,
		RejectedEvents: map[string]int{}, CandidateFailures: map[string]int{},
		CandidateAudits: []CandidateAudit{},
	}
}

func (s *AnalysisSummary) Record(episodeNumber, candidateIndex int, candidate CandidateSegment, evaluation CandidateEvaluation) {
	s.Candidates++
	s.ReturnedEvents += len(evaluation.Decisions)
	s.AcceptedEvents += len(evaluation.Highlights)
	if evaluation.Failure != "" {
		s.CandidateFailures[evaluation.Failure]++
	} else if len(evaluation.Decisions) == 0 {
		s.CandidatesWithoutEvents++
	}
	for _, decision := range evaluation.Decisions {
		if decision.Outcome != "accepted" {
			s.RejectedEvents[decision.Outcome]++
		}
	}
	var diagnostic *CandidateFailureDiagnostic
	if evaluation.FailureDiagnostic != nil {
		value := *evaluation.FailureDiagnostic
		diagnostic = &value
	}
	s.CandidateAudits = append(s.CandidateAudits, CandidateAudit{
		EpisodeNumber: episodeNumber, CandidateIndex: candidateIndex,
		StartSec: candidate.StartSec, EndSec: candidate.EndSec,
		Failure: evaluation.Failure, FailureDiagnostic: diagnostic, Decisions: evaluation.Decisions,
	})
}

type HighlightClipRecord struct {
	GlobalSequence  int      `json:"global_sequence"`
	EpisodeNumber   int      `json:"episode_number"`
	EpisodeSequence int      `json:"episode_sequence"`
	Filename        string   `json:"filename"`
	RelativePath    string   `json:"relative_path"`
	StartSec        float64  `json:"start_sec"`
	EndSec          float64  `json:"end_sec"`
	DurationSec     float64  `json:"duration_sec"`
	Title           string   `json:"title"`
	FinalScore      float64  `json:"final_score"`
	Reason          string   `json:"reason"`
	Tags            []string `json:"tags"`
}

type HighlightManifest struct {
	CreatedAt          string                `json:"created_at"`
	SourceFolder       string                `json:"source_folder"`
	HighlightSetFolder string                `json:"highlight_set_folder"`
	Clips              []HighlightClipRecord `json:"clips"`
	Warnings           []string              `json:"warnings"`
	EpisodeErrors      []string              `json:"episode_errors"`
	AnalysisSummary    AnalysisSummary       `json:"analysis_summary"`
}
