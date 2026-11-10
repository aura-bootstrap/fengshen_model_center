package componentprotocol

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func absFixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func TestDecodeJobRequestVariants(t *testing.T) {
	audio := absFixture(t, "audio.wav")
	reference := absFixture(t, "reference.wav")
	bag := absFixture(t, "bag.yaml")
	model := absFixture(t, "model.bin")
	model2 := absFixture(t, "model2.bin")
	vad := absFixture(t, "vad.bin")
	valid := []struct {
		name string
		req  JobRequest
	}{
		{"whisper-vad", JobRequest{Protocol: BatchHTTPv1, Kind: "whisper-batch", RequestID: "r1", Input: JobInput{AudioPath: audio}, Models: map[string][]string{"whisper-asr": {model}, "whisper-vad": {vad}}}},
		{"demucs", JobRequest{Protocol: TimbreHTTPv1, Kind: "demucs", RequestID: "r2", Input: JobInput{AudioPath: audio}, Models: map[string][]string{"demucs": {model}}, Bag: bag, Options: JobOptions{Device: "cpu"}}},
		{"demucs-ft", JobRequest{Protocol: TimbreHTTPv1, Kind: "demucs", RequestID: "r3", Input: JobInput{AudioPath: audio}, Models: map[string][]string{"demucs-ft": {model, model2, absFixture(t, "m3.th"), absFixture(t, "m4.th")}}, Bag: bag, Options: JobOptions{Device: "cuda"}}},
		{"seedvc", JobRequest{Protocol: TimbreHTTPv1, Kind: "seedvc", RequestID: "r4", Input: JobInput{AudioPath: audio, ReferencePath: reference}, Models: map[string][]string{"seed-vc": {model, model2}, "seed-whisper": {absFixture(t, "w.safetensors")}, "campplus": {absFixture(t, "camp.bin")}, "bigvgan": {absFixture(t, "big.pt")}}, Options: JobOptions{Device: "cpu", Pitch: 0, Steps: 25}}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeJobRequest(mustJSON(t, tc.req)); err != nil {
				t.Fatal(err)
			}
		})
	}

	base := valid[0].req
	invalid := []struct {
		name string
		req  JobRequest
	}{
		{"protocol", func() JobRequest { r := base; r.Protocol = StreamWSv1; return r }()},
		{"kind", func() JobRequest { r := base; r.Kind = "unknown"; return r }()},
		{"missing-asr", func() JobRequest { r := base; r.Models = map[string][]string{}; return r }()},
		{"double-asr", func() JobRequest { r := base; r.Models = map[string][]string{"whisper-asr": {model, model2}}; return r }()},
		{"bad-vad", func() JobRequest {
			r := base
			r.Models = map[string][]string{"whisper-asr": {model}, "whisper-vad": {}}
			return r
		}()},
		{"relative-model", func() JobRequest {
			r := base
			r.Models = map[string][]string{"whisper-asr": {"relative.bin"}}
			return r
		}()},
		{"relative-audio", func() JobRequest { r := base; r.Input.AudioPath = "audio.wav"; return r }()},
		{"reference-on-batch", func() JobRequest { r := base; r.Input.ReferencePath = reference; return r }()},
		{"demucs-bag", func() JobRequest { r := valid[1].req; r.Bag = ""; return r }()},
		{"demucs-device", func() JobRequest { r := valid[1].req; r.Options.Device = "gpu"; return r }()},
		{"demucs-count", func() JobRequest { r := valid[1].req; r.Models = map[string][]string{"demucs": {}}; return r }()},
		{"demucs-ft-count", func() JobRequest { r := valid[2].req; r.Models["demucs-ft"] = r.Models["demucs-ft"][:3]; return r }()},
		{"seedvc-bag", func() JobRequest { r := valid[3].req; r.Bag = bag; return r }()},
		{"seedvc-models", func() JobRequest { r := valid[3].req; r.Models["seed-vc"] = r.Models["seed-vc"][:1]; return r }()},
		{"seedvc-device", func() JobRequest { r := valid[3].req; r.Options.Device = "tpu"; return r }()},
		{"seedvc-steps", func() JobRequest { r := valid[3].req; r.Options.Steps = 24; return r }()},
		{"seedvc-pitch", func() JobRequest { r := valid[3].req; r.Options.Pitch = 25; return r }()},
	}
	for _, tc := range invalid {
		t.Run("invalid-"+tc.name, func(t *testing.T) {
			if _, err := DecodeJobRequest(mustJSON(t, tc.req)); err == nil {
				t.Fatal("invalid job accepted")
			}
		})
	}
}

func TestDecodeLlamaStart(t *testing.T) {
	model := absFixture(t, "model.gguf")
	mmproj := absFixture(t, "mmproj.gguf")
	for _, valid := range []LlamaStartRequest{
		{Protocol: LlamaHTTPv1, Model: model},
		{Protocol: LlamaHTTPv1, Model: model, MMProj: mmproj, Vision: true},
	} {
		if _, err := DecodeLlamaStart(mustJSON(t, valid)); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []LlamaStartRequest{
		{Protocol: StreamWSv1, Model: model},
		{Protocol: LlamaHTTPv1, Model: "relative.gguf"},
		{Protocol: LlamaHTTPv1, Model: model, Vision: true},
		{Protocol: LlamaHTTPv1, Model: model, MMProj: mmproj},
	}
	for _, req := range invalid {
		if _, err := DecodeLlamaStart(mustJSON(t, req)); err == nil {
			t.Fatalf("invalid llama start accepted: %+v", req)
		}
	}
	if _, err := DecodeLlamaStart([]byte(strings.Repeat("x", MaxControlJSON+1))); err == nil {
		t.Fatal("oversized llama start accepted")
	}
}

func TestJobStateAndErrors(t *testing.T) {
	for _, state := range []JobState{JobSucceeded, JobFailed, JobCancelled} {
		if !state.Terminal() {
			t.Fatalf("%s is not terminal", state)
		}
	}
	for _, state := range []JobState{JobRunning, "unknown"} {
		if state.Terminal() {
			t.Fatalf("%s is terminal", state)
		}
	}
	if got := Fail("component_auth"); got.Code != "component_auth" || got.Message == "" || got.Error() != "component_auth" {
		t.Fatalf("Fail=%+v", got)
	}
	if got := Fail("unmapped"); got.Code != "unmapped" || got.Message == "" {
		t.Fatalf("Fail unmapped=%+v", got)
	}
	if Artifacts("unknown") != nil || ArtifactAllowed("unknown", "x") {
		t.Fatal("unknown artifact kind accepted")
	}
	if len(Artifacts("whisper-batch")) != 1 || len(Artifacts("seedvc")) != 1 {
		t.Fatal("artifact list mismatch")
	}
}

func TestStrictJSONAcceptsValidObject(t *testing.T) {
	var value map[string]any
	if err := StrictJSON([]byte(`{"a":1,"b":[2,3]}`), &value); err != nil {
		t.Fatal(err)
	}
}
