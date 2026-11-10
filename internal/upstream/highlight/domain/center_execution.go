package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
)

var ErrCenterExecutionInvalid = errors.New("highlight_center_execution_invalid")

// These are frozen protocol identities, not declarations of delivered adapters.
const (
	CenterWhisperBatchProtocol  = "fengshen.whispercpp.batch/v1"
	CenterWhisperStreamProtocol = "fengshen.whispercpp.stream/v1"
	CenterLlamaProtocol         = "llama.cpp.server.http/b12182"
	CenterDemucsCLIProtocol     = "fengshen.demucs.cli/v1"
	CenterSeedVCCLIProtocol     = "fengshen.seedvc.cli/v1"
)

// FrozenExport is independent of candidate windows and old HighlightConfig hashes.
// Zero values never stand in for a server-issued policy or its reference.
type FrozenExport struct {
	DurationSec int             `json:"duration_sec"`
	Policy      ExportPolicy    `json:"policy"`
	PolicyRef   ExportPolicyRef `json:"policy_ref"`
}

func (f FrozenExport) Validate() error {
	if err := f.Policy.Validate(); err != nil {
		return err
	}
	if err := f.PolicyRef.Validate(); err != nil {
		return err
	}
	_, hash, err := CanonicalExportPolicy(f.Policy)
	if err != nil {
		return err
	}
	if hash != f.PolicyRef.SHA256 {
		return errors.New("highlight_export_policy_changed")
	}
	d := f.Policy.ExportDurationSeconds
	if f.DurationSec < d.Min || f.DurationSec > d.Max {
		return errors.New("highlight_export_duration_invalid")
	}
	return nil
}

// CenterMediaConfig intentionally has no cache, executable or weight path.
type CenterMediaConfig struct {
	SubtitleEnabled bool    `json:"subtitle_enabled"`
	SceneThreshold  float64 `json:"scene_threshold"`
}

type CenterTimbreConfig struct {
	Separate bool `json:"separate"`
	Convert  bool `json:"convert"`
}

// FrozenCenterExecution keeps each client's established analysis wire type while
// sharing the schema2 envelope. B is the lightweight integration reference pair;
// domain does not import either a client, center main or a runtime implementation.
type FrozenCenterExecution[A any, B any] struct {
	Schema             int                `json:"schema"`
	AnalysisChain      string             `json:"analysis_chain"`
	AudioFeatureSchema string             `json:"audio_feature_schema"`
	PromptVersion      string             `json:"prompt_version"`
	ModelExecution     string             `json:"model_execution"`
	ScoreRoute         string             `json:"score_route"`
	Analysis           A                  `json:"analysis"`
	Media              CenterMediaConfig  `json:"media"`
	VADEnabled         bool               `json:"vad_enabled"`
	VisionEnabled      bool               `json:"vision_enabled"`
	Timbre             CenterTimbreConfig `json:"timbre,omitzero"`
	Bindings           []B                `json:"bindings"`
	Export             FrozenExport       `json:"export"`
}

// Validate checks the frozen contract, not availability, authorization or actual
// inference. VAD is part of the transcribe binding, never a standalone scene.
func (s FrozenCenterExecution[A, B]) Validate(analysis func(A) error, binding func(B) (scene, component, protocol string, err error)) error {
	if s.Schema != 2 || s.ModelExecution != "center" || s.ScoreRoute != "center" || !IsAudioTextAnalysis(AnalysisChain(s.AnalysisChain)) {
		return ErrCenterExecutionInvalid
	}
	if err := ValidateAnalysisVersions(AnalysisChain(s.AnalysisChain), s.AudioFeatureSchema, s.PromptVersion); err != nil {
		return err
	}
	if math.IsNaN(s.Media.SceneThreshold) || math.IsInf(s.Media.SceneThreshold, 0) || s.Media.SceneThreshold < 0.05 || s.Media.SceneThreshold > 0.95 {
		return ErrCenterExecutionInvalid
	}
	if err := analysis(s.Analysis); err != nil {
		return err
	}
	if err := s.Export.Validate(); err != nil {
		return err
	}
	if s.Timbre.Convert && !s.Timbre.Separate {
		return ErrCenterExecutionInvalid
	}
	seen := map[string]bool{}
	for _, b := range s.Bindings {
		scene, component, protocol, err := binding(b)
		if err != nil {
			return err
		}
		if seen[scene] {
			return ErrCenterExecutionInvalid
		}
		seen[scene] = true
		switch scene {
		case "highlight.transcribe":
			expected := "whisper-batch"
			if s.AnalysisChain == string(AnalysisChainAudioTextStreamV1) {
				expected = "whisper-stream"
			}
			if component != expected || protocol == "" {
				return ErrCenterExecutionInvalid
			}
			expectedProtocol := CenterWhisperBatchProtocol
			if expected == "whisper-stream" {
				expectedProtocol = CenterWhisperStreamProtocol
			}
			if protocol != expectedProtocol {
				return ErrCenterExecutionInvalid
			}
		case "highlight.score":
			if component != "llama-runtime" || protocol != CenterLlamaProtocol {
				return ErrCenterExecutionInvalid
			}
		case "highlight.vision":
			if !s.VisionEnabled || component != "llama-runtime" || protocol != CenterLlamaProtocol {
				return ErrCenterExecutionInvalid
			}
		case "timbre.separate":
			if !s.Timbre.Separate || component != "demucs-runner" || protocol != CenterDemucsCLIProtocol {
				return ErrCenterExecutionInvalid
			}
		case "timbre.convert":
			if !s.Timbre.Convert || component != "seedvc-runner" || protocol != CenterSeedVCCLIProtocol {
				return ErrCenterExecutionInvalid
			}
		default:
			return ErrCenterExecutionInvalid
		}
	}
	if !seen["highlight.transcribe"] || !seen["highlight.score"] || s.VisionEnabled && !seen["highlight.vision"] || s.Timbre.Separate && !seen["timbre.separate"] || s.Timbre.Convert && !seen["timbre.convert"] {
		return ErrCenterExecutionInvalid
	}
	return nil
}

// DecodeCenterJSON rejects unknown fields, nulls, duplicate keys (including
// nested identities) and trailing JSON. Only schema2 uses this stricter decoder;
// historical snapshot decoding and canonical bytes remain untouched.
func DecodeCenterJSON(data []byte, target any) error {
	check := json.NewDecoder(bytes.NewReader(data))
	if err := centerJSONValue(check); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return ErrCenterExecutionInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func centerJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return ErrCenterExecutionInvalid
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrCenterExecutionInvalid
			}
			seen[name] = true
			if err := centerJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := centerJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return ErrCenterExecutionInvalid
	}
	_, err = d.Token()
	return err
}
