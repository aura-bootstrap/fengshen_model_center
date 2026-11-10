package scoring

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

type LocalVisionConfig struct {
	BaseURL        string        `json:"base_url"`
	APIKey         string        `json:"-"`
	Model          string        `json:"model"`
	TimeoutSec     int           `json:"timeout_sec"`
	MaxRetries     int           `json:"-"`
	DisableRetries bool          `json:"-"`
	RetryDelay     time.Duration `json:"-"`
}

func DefaultLocalVisionConfig() LocalVisionConfig {
	return LocalVisionConfig{BaseURL: LocalBaseURL, TimeoutSec: DefaultTimeoutSec}
}

func (c LocalVisionConfig) clientConfig() Config {
	return Config{
		Provider: ProviderLocal, BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model,
		TimeoutSec: c.TimeoutSec, VisionEnabled: true, MaxRetries: c.MaxRetries, DisableRetries: c.DisableRetries, RetryDelay: c.RetryDelay,
	}
}

func (c LocalVisionConfig) Validate() error {
	return c.clientConfig().Validate()
}

type LocalVisionClient struct {
	client *Client
}

func NewLocalVisionClient(config LocalVisionConfig) (*LocalVisionClient, error) {
	clientConfig := config.clientConfig()
	if err := clientConfig.Validate(); err != nil {
		return nil, newError(ErrorKindLocalVision, 0, config.APIKey, "invalid local vision configuration", SanitizeError(err, config.APIKey))
	}
	client, err := NewClient(clientConfig)
	if err != nil {
		return nil, newError(ErrorKindLocalVision, 0, config.APIKey, "cannot create local vision client", SanitizeError(err, config.APIKey))
	}
	return &LocalVisionClient{client: client}, nil
}

// NewConstrainedLocalVisionClient is for center-owned llama runtimes with GBNF support.
func NewConstrainedLocalVisionClient(config LocalVisionConfig) (*LocalVisionClient, error) {
	client, err := NewLocalVisionClient(config)
	if err != nil {
		return nil, err
	}
	client.client.constrainedVision = true
	return client, nil
}

func constrainedVisionPrompt(request CandidateInput, hasImage bool) string {
	transcript := request.Transcript
	if transcript == "" {
		transcript = speechUnitLines(request.EligibleUnits)
	}
	context := request.Context
	if context == "" {
		context = speechUnitLines(request.ContextUnits)
	}
	image := "none"
	if hasImage {
		image = "3x2 JPEG contact sheet of sampled frames, not continuous video"
	}
	return "Find all distinct attractive events supported inside this candidate; no event quota. Return {\"events\":[...]} or an empty array if none. Never repeat, split one continuous event into duplicates, or join disconnected events.\n" +
		"Each event needs seven scores in [0,1]: highlight_score, conflict_score, emotion_score, reversal_score, visual_score, hook_score, completeness_score; plus start_unit_id, end_unit_id, recommended_start_sec, recommended_end_sec, title, reason, tags, include_in_compilation.\n" +
		"Highlight threshold: " + formatScore(request.MinHighlightScore) + "; completeness threshold: " + formatScore(request.MinCompletenessScore) + ". Attractions include conflict, reversal, wit, humor, warmth, emotional or relationship changes, and substantive suspense. Exclude transitions, repetition and empty exposition. Preserve setup and response needed to understand each attraction; no need to resolve the whole story. Never cut a sentence or omit an essential answer.\n" +
		"For dialogue, select only eligible start/end IDs inclusive and in order; keep intermediate units continuously, set both seconds fields to null. Context IDs are not eligible. Do not invent IDs or force a duration. Exclude events requiring unavailable dialogue.\n" +
		"Only explicit VAD confirmation of a wholly speech-free episode AND an attached image allow visual-only events: both IDs null, seconds within the candidate in increasing order. Sampled frames do not prove continuous action, sound, emotion or speakers. Exclude uncertain action completeness; invent no sound or plot.\n" +
		"Request: " + request.RequestID + "; candidate: " + strconv.Itoa(request.CandidateIndex) + "; episode: " + strconv.Itoa(request.EpisodeNumber) +
		"; seconds: " + strconv.FormatFloat(request.StartSec, 'f', -1, 64) + " to " + strconv.FormatFloat(request.EndSec, 'f', -1, 64) +
		"\nEligible dialogue: " + transcript + "\nRead-only context: " + context + "\nAudio evidence: " + request.AudioSummary +
		"\nShot evidence: " + request.ShotSummary + "\nImage: " + image + "\nClose the array and object after all supported distinct events, then stop."
}

func constrainedVisionGrammar(request CandidateInput) string {
	// Quote the JSON token again as a GBNF literal, never as grammar source.
	literal := func(value string) string {
		token, _ := json.Marshal(value)
		return strconv.Quote(string(token))
	}
	// Scores use plain decimals with no precision cap; exponents cannot bypass [0,1].
	// Seconds and JSON char escaping follow fd27b20; time ranges stay runtime-validated.
	var grammar strings.Builder
	grammar.WriteString(`root ::= "{" space "\"events\"" space ":" space "[" space (event ("," space event){0,127})? "]" space "}" space
score ::= ("0" ("." [0-9]+)? | "1" ("." "0"+)?) space
space ::= [ \t\n\r]*
text ::= "\"" char{1,8192} "\"" space
char ::= [^"\\\x7F\x00-\x1F] | [\\] (["\\bfnrt] | "u" [0-9a-fA-F]{4})
tags ::= "[" space (text ("," space text){0,127})? "]" space
boolean ::= ("true" | "false") space
number ::= ("-"? integral-part) ("." decimal-part)? ([eE] [-+]? integral-part)?
integral-part ::= [0] | [1-9] [0-9]{0,15}
decimal-part ::= [0-9]{1,16}
`)
	if len(request.EligibleUnits) == 0 {
		grammar.WriteString("seconds ::= number space\nunit-id ::= (\"null\"")
	} else {
		grammar.WriteString("seconds ::= \"null\" space\nunit-id ::= (")
		for i, unit := range request.EligibleUnits {
			if i > 0 {
				grammar.WriteString(" | ")
			}
			grammar.WriteString(literal(unit.ID))
		}
	}
	grammar.WriteString(") space\nevent ::= \"{\" space ")
	fields := []struct{ name, rule string }{
		{"highlight_score", "score"}, {"conflict_score", "score"}, {"emotion_score", "score"},
		{"reversal_score", "score"}, {"visual_score", "score"}, {"hook_score", "score"}, {"completeness_score", "score"},
		{"start_unit_id", "unit-id"}, {"end_unit_id", "unit-id"},
		{"recommended_start_sec", "seconds"}, {"recommended_end_sec", "seconds"},
		{"title", "text"}, {"reason", "text"}, {"tags", "tags"}, {"include_in_compilation", "boolean"},
	}
	for i, field := range fields {
		if i > 0 {
			grammar.WriteString(` "," space `)
		}
		grammar.WriteString(literal(field.name) + ` space ":" space ` + field.rule)
	}
	grammar.WriteString(" \"}\" space\n")
	return grammar.String()
}

func (c *LocalVisionClient) ScoreEvents(ctx context.Context, request CandidateInput) ([]*RawEvent, error) {
	if domain.IsAudioTextAnalysis(request.AnalysisChain) || request.AudioFeatures != nil {
		return nil, newError(ErrorKindLocalVision, 0, c.client.config.APIKey, "local vision requires a separate visual review request", nil)
	}
	return c.client.ScoreEvents(ctx, request)
}
