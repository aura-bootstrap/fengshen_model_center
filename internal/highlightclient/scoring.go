package highlightclient

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

type scorer struct {
	client *Client
	scene  string
}

func (s *scorer) ScoreEvents(ctx context.Context, input scoring.CandidateInput) ([]*scoring.RawEvent, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if s.scene != "highlight.score" && s.scene != "highlight.vision" {
		return nil, errBinding
	}
	b, e := s.client.binding(s.scene, "llama-runtime")
	if e != nil {
		return nil, e
	}
	if e = input.ValidateAnalysisInput(); e != nil {
		return nil, e
	}
	if s.scene == "highlight.score" && (input.Image != nil || input.ContactSheetPath != "") {
		return nil, errInput
	}
	if s.scene == "highlight.vision" && (domain.IsAudioTextAnalysis(input.AnalysisChain) || input.AudioFeatures != nil) {
		return nil, errInput
	}
	id, e := requestID()
	if e != nil {
		return nil, e
	}
	g := s.client.grant()
	if input.TaskID != "" && input.TaskID != g.TaskID {
		return nil, errInput
	}
	// JSON is the shared scoring projection. Remove Image before marshaling so
	// neither base64 nor local media fields can reach the runtime score DTO.
	projected := input
	projected.Image = nil
	projected.RequestID = id
	projected.TaskID = g.TaskID
	raw, e := json.Marshal(projected)
	if e != nil || len(raw) > 64<<10 {
		return nil, errInput
	}
	var candidate integration.RuntimeScoreInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&candidate); e != nil {
		return nil, errInput
	}
	body := integration.RuntimeBody{Component: b.Component}
	if s.scene == "highlight.vision" {
		a, e := s.client.uploadImage(ctx, input)
		if e != nil {
			return nil, e
		}
		body.Vision = &integration.RuntimeVisionInput{AssetID: a.ID, Candidate: candidate}
	} else {
		body.Score = &candidate
	}
	result, e := s.client.run(ctx, b, id, body, nil, func(r integration.RuntimeResult) error {
		if r.Events == nil || len(r.Events) > 128 || r.Items != nil || r.Stream != nil || r.Media != nil || r.Face != "" {
			return errResult
		}
		for _, v := range r.Events {
			if !validRawEvent(v, candidate) {
				return errResult
			}
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if result.Events == nil || len(result.Events) > 128 || result.Items != nil || result.Stream != nil || result.Media != nil || result.Face != "" {
		return nil, errResult
	}
	events := make([]*scoring.RawEvent, len(result.Events))
	for i, v := range result.Events {
		if !validRawEvent(v, candidate) {
			return nil, errResult
		}
		raw, e = json.Marshal(v)
		if e != nil {
			return nil, errResult
		}
		if e = json.Unmarshal(raw, &events[i]); e != nil {
			return nil, errResult
		}
	}
	// Business filtering remains in the existing scoring service, unchanged.
	return events, nil
}
func validRawEvent(v *integration.RuntimeRawEvent, in integration.RuntimeScoreInput) bool {
	if v == nil {
		return false
	}
	for _, n := range []float64{v.HighlightScore, v.ConflictScore, v.EmotionScore, v.ReversalScore, v.VisualScore, v.HookScore, v.CompletenessScore} {
		if !domain.Finite(n) || n < 0 || n > 1 {
			return false
		}
	}
	if strings.TrimSpace(v.Title) == "" || strings.TrimSpace(v.Reason) == "" || v.Tags == nil || len(v.Tags) > 128 || (v.StartUnitID == nil) != (v.EndUnitID == nil) || (v.RecommendedStartSec == nil) != (v.RecommendedEndSec == nil) {
		return false
	}
	for _, tag := range v.Tags {
		if strings.TrimSpace(tag) == "" {
			return false
		}
	}
	if v.StartUnitID != nil {
		a, b := -1, -1
		for i, u := range in.EligibleUnits {
			if u.ID == *v.StartUnitID {
				a = i
			}
			if u.ID == *v.EndUnitID {
				b = i
			}
		}
		if a < 0 || b < a {
			return false
		}
	}
	return v.RecommendedStartSec == nil || domain.ValidRange(*v.RecommendedStartSec, *v.RecommendedEndSec) && *v.RecommendedStartSec >= in.StartSec && *v.RecommendedEndSec <= in.EndSec
}
