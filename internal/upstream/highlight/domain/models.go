package domain

import (
	"fmt"
	"math"
	"strings"
)

type Episode struct {
	EpisodeNumber int    `json:"episode_number"`
	VideoPath     string `json:"video_path"`
}

type TranscriptWord struct {
	StartSec    float64  `json:"start_sec"`
	EndSec      float64  `json:"end_sec"`
	Text        string   `json:"text"`
	Probability *float64 `json:"probability,omitempty"`
}

func (w TranscriptWord) Validate() error {
	if !ValidRange(w.StartSec, w.EndSec) {
		return fmt.Errorf("语音时间范围无效")
	}
	if w.Probability != nil && (!Finite(*w.Probability) || *w.Probability < 0 || *w.Probability > 1) {
		return fmt.Errorf("词置信度必须位于 0 到 1")
	}
	return nil
}

type TranscriptItem struct {
	StartSec    float64          `json:"start_sec"`
	EndSec      float64          `json:"end_sec"`
	Text        string           `json:"text"`
	Probability *float64         `json:"probability,omitempty"`
	Words       []TranscriptWord `json:"words"`
}

func (i TranscriptItem) Validate() error {
	if !ValidRange(i.StartSec, i.EndSec) {
		return fmt.Errorf("字幕时间范围无效")
	}
	if strings.TrimSpace(i.Text) == "" {
		return fmt.Errorf("字幕文本不能为空")
	}
	for _, word := range i.Words {
		if err := word.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Transcript struct {
	Items             []TranscriptItem `json:"items"`
	Source            string           `json:"source"`
	ConfirmedNoSpeech bool             `json:"confirmed_no_speech"`
	// NoSpeechEvidence is populated only by a backend that ran a reliable VAD.
	// It is deliberately not inferred from an empty ASR transcription.
	NoSpeechEvidence *NoSpeechEvidence `json:"no_speech_evidence,omitempty"`
}

// NoSpeechEvidence is explicit, machine-checkable VAD evidence. An empty
// whisper-cli result never synthesizes this value.
type NoSpeechEvidence struct {
	Backend  string `json:"backend"`
	Reliable bool   `json:"reliable"`
}

// ConfirmsNoSpeech requires explicit reliable VAD evidence. Nil, unreliable or
// unidentified evidence can never be promoted to confirmed no-speech input.
func (e *NoSpeechEvidence) ConfirmsNoSpeech() bool {
	return e != nil && e.Reliable && strings.TrimSpace(e.Backend) != ""
}

// Diagnostic is safe to persist: callers must provide a stable code and a
// sanitized message rather than raw provider or process output.
type Diagnostic struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	EpisodeNumber  int    `json:"episode_number,omitempty"`
	CandidateIndex int    `json:"candidate_index,omitempty"`
}

func (t Transcript) Validate() error {
	for _, item := range t.Items {
		if err := item.Validate(); err != nil {
			return err
		}
	}
	if len(t.Items) > 0 && t.ConfirmedNoSpeech {
		return fmt.Errorf("有台词时不得确认无语音")
	}
	if t.ConfirmedNoSpeech && !t.NoSpeechEvidence.ConfirmsNoSpeech() {
		return fmt.Errorf("确认无语音需要可靠 VAD 证据")
	}
	return nil
}

type CandidateSegment struct {
	EpisodeNumber int      `json:"episode_number"`
	StartSec      float64  `json:"start_sec"`
	EndSec        float64  `json:"end_sec"`
	SourceSignals []string `json:"source_signals"`
	LocalScore    float64  `json:"local_score"`
}

func (c CandidateSegment) Validate() error {
	if c.EpisodeNumber < 1 || !ValidRange(c.StartSec, c.EndSec) {
		return fmt.Errorf("候选范围无效")
	}
	return nil
}

type SpeechUnit struct {
	ID                string           `json:"id"`
	StartSec          float64          `json:"start_sec"`
	EndSec            float64          `json:"end_sec"`
	Text              string           `json:"text"`
	Words             []TranscriptWord `json:"words"`
	AlignmentReliable bool             `json:"alignment_reliable"`
}

func (u SpeechUnit) Line() string {
	return fmt.Sprintf("%s [%.3f-%.3fs] %s", u.ID, u.StartSec, u.EndSec, u.Text)
}

// SourceCutEvidence records the exact source frame interval approved by review.
// PTS values are strings to preserve integer precision in persisted JSON.
type SourceCutEvidence struct {
	Schema        int     `json:"schema"`
	SourceVersion string  `json:"source_version"`
	TimeBaseNum   int64   `json:"time_base_num"`
	TimeBaseDen   int64   `json:"time_base_den"`
	OriginSeconds float64 `json:"origin_seconds"`
	StartPTS      string  `json:"start_pts"`
	EndPTS        string  `json:"end_pts"`
	StartSeconds  float64 `json:"start_seconds"`
	EndSeconds    float64 `json:"end_seconds"`
	FrameSHA256   string  `json:"frame_sha256"`
}

type HighlightSegment struct {
	EpisodeNumber int                `json:"episode_number"`
	StartSec      float64            `json:"start_sec"`
	EndSec        float64            `json:"end_sec"`
	Title         string             `json:"title"`
	FinalScore    float64            `json:"final_score"`
	Reason        string             `json:"reason"`
	Tags          []string           `json:"tags"`
	ClipPath      string             `json:"clip_path,omitempty"`
	SourceCut     *SourceCutEvidence `json:"source_cut,omitempty"`
}

func (h HighlightSegment) Validate() error {
	if h.EpisodeNumber < 1 || !ValidRange(h.StartSec, h.EndSec) {
		return fmt.Errorf("高光范围无效")
	}
	if err := ValidateScore(h.FinalScore); err != nil {
		return err
	}
	if strings.TrimSpace(h.Title) == "" || strings.TrimSpace(h.Reason) == "" {
		return fmt.Errorf("标题和理由不能为空")
	}
	for _, tag := range h.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("标签不能为空")
		}
	}
	return nil
}

func ValidRange(start, end float64) bool {
	return finite(start) && finite(end) && start >= 0 && end > start
}

func ValidateScore(score float64) error {
	if !finite(score) || score < 0 || score > 1 {
		return fmt.Errorf("评分必须位于 0 到 1")
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func Finite(value float64) bool { return finite(value) }
