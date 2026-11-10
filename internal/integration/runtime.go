package integration

import (
	"errors"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	DemucsCLIProtocol = domain.CenterDemucsCLIProtocol
	SeedVCCLIProtocol = domain.CenterSeedVCCLIProtocol
	RuntimeProtocol   = "fengshen.model-center.runtime/v1"

	// Session and task headers are non-secret routing identifiers, not credentials.
	RuntimeSessionHeader    = "X-Model-Center-Session-ID"
	RuntimeTaskHeader       = "X-Model-Center-Task-ID"
	RuntimeCapabilitiesPath = "/api/integration/runtime-capabilities"
	BindingsPath            = "/api/bindings"
	RuntimeAssetsPath       = "/api/runtime/assets"
	RuntimeJobsPath         = "/api/runtime/jobs"
)

type SceneDefinition struct {
	ID                string     `json:"id"`
	RequiredModels    []string   `json:"required_models"`
	OptionalModels    []string   `json:"optional_models"`
	AlternativeModels [][]string `json:"alternative_models,omitempty"`
	Components        []string   `json:"components"`
}

func RuntimeScenes() []SceneDefinition {
	return []SceneDefinition{
		{ID: "highlight.transcribe", RequiredModels: []string{"whisper-asr"}, OptionalModels: []string{"whisper-vad"}, Components: []string{"whisper-batch", "whisper-stream"}},
		{ID: "highlight.score", RequiredModels: []string{"local-text"}, Components: []string{"llama-runtime"}},
		{ID: "highlight.vision", RequiredModels: []string{"local-vision"}, Components: []string{"llama-runtime"}},
		{ID: "timbre.separate", RequiredModels: []string{"demucs"}, OptionalModels: []string{"demucs-ft"}, AlternativeModels: [][]string{{"demucs"}, {"demucs-ft"}}, Components: []string{"demucs-runner"}},
		{ID: "timbre.convert", RequiredModels: []string{"seed-vc", "seed-whisper", "campplus", "bigvgan"}, Components: []string{"seedvc-runner"}},
		{ID: "image.face", RequiredModels: []string{"haar-frontal", "haar-profile"}, Components: []string{"haar-consumer"}},
	}
}

func RuntimeScene(id string) (SceneDefinition, bool) {
	for _, scene := range RuntimeScenes() {
		if scene.ID == id {
			return scene, true
		}
	}
	return SceneDefinition{}, false
}

func opaqueRuntimeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func RuntimeRouteAllowed(method, path string) bool {
	if path == RuntimeAssetsPath || path == RuntimeJobsPath {
		return method == "POST"
	}
	if !strings.HasPrefix(path, RuntimeJobsPath+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, RuntimeJobsPath+"/"), "/")
	if !opaqueRuntimeID(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return method == "GET"
	}
	if len(parts) == 2 {
		return parts[1] == "events" && method == "GET" || (parts[1] == "cancel" || parts[1] == "ack") && method == "POST"
	}
	return len(parts) == 3 && parts[1] == "results" && opaqueRuntimeID(parts[2]) && method == "GET"
}

type ExecutionBindingRef struct {
	Scene    string `json:"scene"`
	Revision uint64 `json:"revision"`
	SHA256   string `json:"sha256"`
}

func (r ExecutionBindingRef) Validate() error {
	if _, ok := RuntimeScene(r.Scene); !ok || r.Revision == 0 || !digestValid(r.SHA256) {
		return errors.New("center_binding_invalid")
	}
	return nil
}

type ExecutionComponentRef struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Protocol       string `json:"protocol"`
}

// WhisperVADSupported describes component support, not the binding's VAD selection.
// Callers must validate the component and protocol separately.
func WhisperVADSupported(component ExecutionComponentRef) bool {
	return component.ID == "whisper-stream" || component.ID == "whisper-batch" && component.Version == "4979e04-batch-v2"
}

func componentVersionValid(version string) bool {
	if len(version) == 0 || len(version) > 128 || strings.Contains(version, "..") || strings.HasSuffix(version, ".") {
		return false
	}
	for i, c := range version {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '_' || c == '-')) {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(version, ".", 2)[0])
	return stem != "CON" && stem != "PRN" && stem != "AUX" && stem != "NUL" && !(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9')
}

func (r ExecutionComponentRef) Validate(scene string) error {
	definition, ok := RuntimeScene(scene)
	if !ok || !componentVersionValid(r.Version) || !digestValid(r.ManifestSHA256) || r.Protocol == "" || !safeCode(r.Protocol) {
		return errors.New("center_component_invalid")
	}
	for _, id := range definition.Components {
		if r.ID == id {
			return nil
		}
	}
	return errors.New("center_component_invalid")
}

type CapabilityState string

const (
	CapabilityMissingBinding   CapabilityState = "missing_binding"
	CapabilityMissingComponent CapabilityState = "missing_component"
	CapabilityIncompatible     CapabilityState = "incompatible"
	CapabilityAvailable        CapabilityState = "available"
	CapabilityBusy             CapabilityState = "busy"
	CapabilityFailed           CapabilityState = "failed"
)

type InferenceEvidence struct {
	State        string                `json:"state"`
	JobID        string                `json:"job_id"`
	InputSHA256  string                `json:"input_sha256"`
	Binding      ExecutionBindingRef   `json:"binding"`
	Component    ExecutionComponentRef `json:"component"`
	StartedAt    time.Time             `json:"started_at"`
	FinishedAt   time.Time             `json:"finished_at"`
	ResultSHA256 string                `json:"result_sha256"`
}

func (e InferenceEvidence) Validate(scene string) error {
	if e.State != "passed" && e.State != "failed" || !opaqueRuntimeID(e.JobID) || !digestValid(e.InputSHA256) || e.Binding.Scene != scene || e.Binding.Validate() != nil || e.Component.Validate(scene) != nil || e.StartedAt.IsZero() || e.FinishedAt.Before(e.StartedAt) || e.State == "passed" && !digestValid(e.ResultSHA256) {
		return errors.New("center_inference_evidence_invalid")
	}
	return nil
}

type RuntimeCapability struct {
	Scene      string                 `json:"scene"`
	State      CapabilityState        `json:"state"`
	Binding    *ExecutionBindingRef   `json:"binding,omitempty"`
	Component  *ExecutionComponentRef `json:"component,omitempty"`
	Inference  *InferenceEvidence     `json:"inference,omitempty"`
	VADEnabled *bool                  `json:"vad_enabled,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
}

func (c RuntimeCapability) Validate() error {
	if _, ok := RuntimeScene(c.Scene); !ok || !safeCode(c.Reason) || c.VADEnabled != nil && c.Scene != "highlight.transcribe" {
		return errors.New("center_capability_invalid")
	}
	switch c.State {
	case CapabilityMissingBinding, CapabilityMissingComponent, CapabilityIncompatible, CapabilityAvailable, CapabilityBusy, CapabilityFailed:
	default:
		return errors.New("center_capability_invalid")
	}
	if c.Binding != nil && (c.Binding.Scene != c.Scene || c.Binding.Validate() != nil) {
		return errors.New("center_capability_invalid")
	}
	if c.Component != nil && c.Component.Validate(c.Scene) != nil {
		return errors.New("center_capability_invalid")
	}
	if (c.State == CapabilityAvailable || c.State == CapabilityBusy) && (c.Binding == nil || c.Component == nil) {
		return errors.New("center_capability_invalid")
	}
	if c.Inference != nil {
		if c.Inference.Validate(c.Scene) != nil || c.Binding == nil || c.Component == nil || c.Inference.Binding != *c.Binding || c.Inference.Component != *c.Component {
			return errors.New("center_inference_evidence_invalid")
		}
	}
	return nil
}

type RuntimeCapabilities struct {
	Schema       int                 `json:"schema"`
	InstanceID   string              `json:"instance_id"`
	Build        string              `json:"build"`
	ObservedAt   time.Time           `json:"observed_at"`
	Capabilities []RuntimeCapability `json:"capabilities"`
}
