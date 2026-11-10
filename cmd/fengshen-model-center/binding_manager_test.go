package main

import (
	"context"
	"errors"
	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

func bindingSelection(t *testing.T, m *Manager, scene, component string, models ...string) BindingSelection {
	t.Helper()
	inv := scanFixture(t, m)
	request := BindingSelection{InventoryID: inv.ID, Component: integration.ExecutionComponentRef{ID: component, Version: "dev-local", ManifestSHA256: strings.Repeat("a", 64), Protocol: "fengshen.test/v1"}, Device: "cpu"}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range m.inventories[inv.ID].Models {
		if contains(models, model.ID) {
			for _, file := range model.files {
				request.Files = append(request.Files, BindingFileSelection{ModelID: model.ID, FileID: file.ID})
			}
		}
	}
	return request
}

func TestBindingExplicitWhisperSelection(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "small")
	put(t, filepath.Join(m.root, "models", "ggml-large-v3-turbo.bin"), "turbo")
	request := bindingSelection(t, m, "highlight.transcribe", "whisper-batch", "whisper-asr")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.resolveBindingLocked(context.Background(), "highlight.transcribe", request); err == nil {
		t.Fatal("two ASR alternatives were silently selected")
	}
	request.Files = request.Files[:1]
	binding, err := m.resolveBindingLocked(context.Background(), "highlight.transcribe", request)
	if err != nil || len(binding.Files) != 1 || binding.Files[0].Identity == "" {
		t.Fatalf("explicit selection: %+v %v", binding, err)
	}
	request.Files = append(request.Files, request.Files[0])
	if _, err := m.resolveBindingLocked(context.Background(), "highlight.transcribe", request); err == nil {
		t.Fatal("duplicate file accepted")
	}
}

func TestBindingCompleteSameGroup(t *testing.T) {
	for _, tc := range []struct {
		name, scene, component, model string
		files                         []string
	}{
		{"text", "highlight.score", "llama-runtime", "local-text", []string{"qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf", "qwen2.5-7b-instruct-q4_k_m-00002-of-00002.gguf"}},
		{"ft", "timbre.separate", "demucs-runner", "demucs-ft", []string{"f7e0c4bc-a.th", "d12395a8-a.th", "92cfc3b6-a.th", "04573f0d-a.th"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManager(t)
			for _, name := range tc.files {
				put(t, filepath.Join(m.root, "models", name), "weight")
			}
			request := bindingSelection(t, m, tc.scene, tc.component, tc.model)
			m.mu.Lock()
			if _, err := m.resolveBindingLocked(context.Background(), tc.scene, request); err != nil {
				t.Fatal(err)
			}
			incomplete := request
			incomplete.Files = incomplete.Files[:len(incomplete.Files)-1]
			if _, err := m.resolveBindingLocked(context.Background(), tc.scene, incomplete); err == nil {
				t.Fatal("incomplete set accepted")
			}
			m.mu.Unlock()
			first := filepath.Join(m.root, "models", tc.files[0])
			other := filepath.Join(m.root, "models", "other", tc.files[0])
			put(t, other, "weight")
			if err := os.Remove(first); err != nil {
				t.Fatal(err)
			}
			request = bindingSelection(t, m, tc.scene, tc.component, tc.model)
			m.mu.Lock()
			defer m.mu.Unlock()
			if _, err := m.resolveBindingLocked(context.Background(), tc.scene, request); err == nil {
				t.Fatal("cross-directory assembly accepted")
			}
		})
	}
}

func TestBindingVisionProjectorComposition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []string
		allowed bool
	}{
		{"complete", []string{"rev-a/SmolVLM-500M-Instruct-Q8_0.gguf", "rev-a/mmproj-SmolVLM-500M-Instruct-f16.gguf"}, true},
		{"missing-projector", []string{"rev-a/SmolVLM-500M-Instruct-Q8_0.gguf"}, false},
		{"missing-model", []string{"rev-a/mmproj-SmolVLM-500M-Instruct-f16.gguf"}, false},
		{"mixed-revisions", []string{"rev-a/SmolVLM-500M-Instruct-Q8_0.gguf", "rev-b/mmproj-SmolVLM-500M-Instruct-f16.gguf"}, false},
		{"ambiguous-revisions", []string{"rev-a/SmolVLM-500M-Instruct-Q8_0.gguf", "rev-a/mmproj-SmolVLM-500M-Instruct-f16.gguf", "rev-b/SmolVLM-500M-Instruct-Q8_0.gguf", "rev-b/mmproj-SmolVLM-500M-Instruct-f16.gguf"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManager(t)
			for _, file := range tc.files {
				put(t, filepath.Join(m.root, "models", "models--ggml-org--SmolVLM-500M-Instruct-GGUF", "snapshots", filepath.FromSlash(file)), "weight")
			}
			request := bindingSelection(t, m, "highlight.vision", "llama-runtime", "local-vision")
			if len(request.Files) != len(tc.files) {
				t.Fatal("fixture files were not recognized")
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			binding, err := m.resolveBindingLocked(context.Background(), "highlight.vision", request)
			if (err == nil) != tc.allowed {
				t.Fatalf("composition accepted=%v, want %v", err == nil, tc.allowed)
			}
			if tc.allowed && (len(binding.Files) != 2 || binding.Files[0].Group != binding.Files[1].Group) {
				t.Fatal("complete vision binding lost its projector group")
			}
		})
	}
}

func TestBindingVisionExactAlternatives(t *testing.T) {
	model, _ := findModel("local-vision")
	for i, pair := range knownVisionPairs {
		for _, mode := range []string{"complete", "mixed-projector", "mixed-model", "both-pairs"} {
			t.Run(pair.model+"/"+mode, func(t *testing.T) {
				names := []string{pair.model, pair.projector}
				other := knownVisionPairs[1-i]
				switch mode {
				case "mixed-projector":
					names[1] = other.projector
				case "mixed-model":
					names[0] = other.model
				case "both-pairs":
					names = append(names, other.model, other.projector)
				}
				var files []*ModelFile
				for _, name := range names {
					files = append(files, &ModelFile{Path: filepath.Join(t.TempDir(), name)})
				}
				if got := bindingCompositionComplete(model, files); got != (mode == "complete") {
					t.Fatalf("exact alternative complete=%v", got)
				}
			})
		}
	}
}

func TestBindingSeedSuiteExplicitComposition(t *testing.T) {
	for _, mutation := range []string{"complete", "missing-seed-vc", "missing-seed-whisper", "missing-campplus", "missing-bigvgan", "unrelated-model", "duplicate-component", "wrong-model-reference", "ambiguous-seed-vc"} {
		t.Run(mutation, func(t *testing.T) {
			m := fixtureManager(t)
			files := map[string]string{
				"seed-vc":      "seed/DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth",
				"seed-whisper": "models--openai--whisper-small/snapshots/rev-a/model.safetensors",
				"campplus":     "speaker/campplus_cn_common.bin",
				"bigvgan":      "models--nvidia--bigvgan_v2_22khz_80band_256x/snapshots/rev-b/bigvgan_generator.pt",
			}
			for _, file := range files {
				put(t, filepath.Join(m.root, "models", filepath.FromSlash(file)), "weight")
			}
			put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "asr")
			if mutation == "ambiguous-seed-vc" {
				put(t, filepath.Join(m.root, "models", "other", filepath.Base(files["seed-vc"])), "other-weight")
			}
			models := []string{"seed-vc", "seed-whisper", "campplus", "bigvgan"}
			if mutation == "unrelated-model" {
				models = append(models, "whisper-asr")
			}
			request := bindingSelection(t, m, "timbre.convert", "seedvc-runner", models...)
			if mutation != "unrelated-model" && mutation != "ambiguous-seed-vc" && len(request.Files) != 4 {
				t.Fatal("fixture suite was not fully recognized")
			}
			if strings.HasPrefix(mutation, "missing-") {
				model := strings.TrimPrefix(mutation, "missing-")
				kept := make([]BindingFileSelection, 0, len(request.Files)-1)
				for _, file := range request.Files {
					if file.ModelID != model {
						kept = append(kept, file)
					}
				}
				request.Files = kept
			}
			if mutation == "duplicate-component" {
				request.Files = append(request.Files, request.Files[0])
			}
			if mutation == "wrong-model-reference" {
				if request.Files[0].ModelID == "campplus" {
					request.Files[0].ModelID = "bigvgan"
				} else {
					request.Files[0].ModelID = "campplus"
				}
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			binding, err := m.resolveBindingLocked(context.Background(), "timbre.convert", request)
			if (err == nil) != (mutation == "complete") {
				t.Fatalf("suite accepted=%v for %s", err == nil, mutation)
			}
			if err == nil && len(binding.Files) != 4 {
				t.Fatal("complete suite lost a component")
			}
		})
	}
}

func TestBindingManagerPersistence(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "small")
	request := bindingSelection(t, m, "highlight.transcribe", "whisper-batch", "whisper-asr")
	snapshot, err := m.PutBinding(context.Background(), "highlight.transcribe", request)
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("save: %+v %v", snapshot, err)
	}
	frozen := m.bindingStore.Snapshot().Bindings["highlight.transcribe"]
	if frozen.Files[0].ScopeIdentity == "" || frozen.Ref.Validate() != nil {
		t.Fatal("missing frozen identity")
	}
	if _, err = m.PutBinding(context.Background(), "highlight.transcribe", request); !errors.Is(err, bindings.ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	view := m.BindingSnapshot()
	view.Bindings["highlight.transcribe"] = BindingView{}
	if m.BindingSnapshot().Bindings["highlight.transcribe"].Ref != frozen.Ref {
		t.Fatal("view mutation changed persistent binding")
	}
	if _, err = m.DeleteBinding(context.Background(), "highlight.transcribe", 1); err != nil {
		t.Fatal(err)
	}
	if len(m.BindingSnapshot().Bindings) != 0 {
		t.Fatal("delete retained binding")
	}
}

func TestBindingCapabilitiesDoNotInventRuntime(t *testing.T) {
	m := fixtureManager(t)
	m.beforeScan = func(context.Context) { t.Fatal("capabilities scanned models") }
	value, err := m.runtimeCapabilities()
	if err != nil || len(value.Capabilities) != 6 {
		t.Fatalf("capabilities: %+v %v", value, err)
	}
	for _, capability := range value.Capabilities {
		if capability.State != integration.CapabilityMissingBinding || capability.Inference != nil {
			t.Fatal("asset observation invented inference")
		}
	}
	m.beforeScan = nil
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "small")
	request := bindingSelection(t, m, "highlight.transcribe", "whisper-batch", "whisper-asr")
	if _, err := m.PutBinding(context.Background(), "highlight.transcribe", request); err != nil {
		t.Fatal(err)
	}
	value, err = m.runtimeCapabilities()
	if err != nil || value.Capabilities[0].State != integration.CapabilityMissingComponent || value.Capabilities[0].Inference != nil {
		t.Fatalf("binding is not delivery: %+v %v", value, err)
	}
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "changed-size")
	value, err = m.runtimeCapabilities()
	if err != nil || value.Capabilities[0].Reason != "center_binding_stale" {
		t.Fatalf("changed file retained capability: %+v %v", value, err)
	}
}

func TestBindingCapabilitiesVADProjection(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "unselected", true: "selected"}[selected], func(t *testing.T) {
			m := fixtureManager(t)
			put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "asr")
			put(t, filepath.Join(m.root, "models", "ggml-silero-v5.1.2.bin"), "vad")
			models := []string{"whisper-asr"}
			if selected {
				models = append(models, "whisper-vad")
			}
			request := bindingSelection(t, m, "highlight.transcribe", "whisper-batch", models...)
			snapshot, err := m.PutBinding(context.Background(), "highlight.transcribe", request)
			if err != nil {
				t.Fatal(err)
			}
			ref := snapshot.Bindings["highlight.transcribe"].Ref
			check := func() {
				t.Helper()
				caps, err := m.runtimeCapabilities()
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range caps.Capabilities {
					if c.Scene != "highlight.transcribe" {
						if c.VADEnabled != nil {
							t.Fatal("non-transcribe capability has VAD projection")
						}
						continue
					}
					if c.VADEnabled == nil || *c.VADEnabled != selected || c.Binding == nil || *c.Binding != ref || c.Validate() != nil {
						t.Fatalf("incorrect binding projection: %+v", c)
					}
					*c.VADEnabled = !selected
				}
			}
			check()
			check()
			put(t, filepath.Join(m.root, "models", "ggml-silero-v5.1.2.bin"), "changed-vad")
			check()
			if m.BindingSnapshot().Bindings["highlight.transcribe"].Ref != ref {
				t.Fatal("projection rewrote binding revision/hash")
			}
			if _, err := m.DeleteBinding(context.Background(), "highlight.transcribe", snapshot.Revision); err != nil {
				t.Fatal(err)
			}
			caps, err := m.runtimeCapabilities()
			if err != nil || caps.Capabilities[0].VADEnabled != nil {
				t.Fatal("missing binding invented known VAD projection")
			}
		})
	}
}

func TestComponentCapabilityVADVersion(t *testing.T) {
	for _, version := range []string{"4979e04-batch-v1", "4979e04-batch-v2", "unknown", "4979e04-batch-v2-extra"} {
		for _, selected := range []bool{false, true} {
			b := bindings.Binding{Scene: "highlight.transcribe", Component: integration.ExecutionComponentRef{ID: "whisper-batch", Version: version, ManifestSHA256: strings.Repeat("a", 64), Protocol: "fengshen.whispercpp.batch/v1"}}
			if selected {
				b.Files = append(b.Files, bindings.FrozenFile{ModelID: "whisper-vad"})
			}
			evidence := components.Evidence{ComponentID: b.Component.ID, Version: version, ManifestSHA256: b.Component.ManifestSHA256, Manifest: &components.Manifest{Protocols: []string{b.Component.Protocol}}, FilesState: "complete", Platform: "matched", Integrity: "matched", StartupTest: "passed"}
			want, reason := integration.CapabilityAvailable, ""
			if selected && version != "4979e04-batch-v2" {
				want, reason = integration.CapabilityIncompatible, "center_whisper_vad_unsupported"
			}
			if state, got := componentCapability(b, []components.Evidence{evidence}); state != want || got != reason {
				t.Errorf("%s selected=%v: %s %s", version, selected, state, got)
			}
		}
	}
}

func TestComponentCapabilityAdapterAdmission(t *testing.T) {
	for _, tc := range []struct{ scene, id, protocol string }{
		{"highlight.transcribe", "whisper-batch", "fengshen.whispercpp.batch/v1"},
		{"highlight.transcribe", "whisper-stream", "fengshen.whispercpp.stream/v1"},
		{"highlight.score", "llama-runtime", "llama.cpp.server.http/b12182"},
		{"highlight.vision", "llama-runtime", "llama.cpp.server.http/b12182"},
		{"image.face", "haar-consumer", "haar/v1"},
	} {
		t.Run(tc.scene+tc.id, func(t *testing.T) {
			binding := bindings.Binding{Scene: tc.scene, Component: integration.ExecutionComponentRef{ID: tc.id, Version: "1", ManifestSHA256: strings.Repeat("a", 64), Protocol: tc.protocol}}
			good := components.Evidence{ComponentID: tc.id, Version: "1", ManifestSHA256: binding.Component.ManifestSHA256, Manifest: &components.Manifest{ComponentID: tc.id, Version: "1", Protocols: []string{tc.protocol}}, FilesState: "complete", Platform: "matched", Integrity: "matched", StartupTest: "passed", Inference: "not_tested"}
			if state, reason := componentCapability(binding, []components.Evidence{good}); state != integration.CapabilityAvailable || reason != "" {
				t.Fatalf("verified supported adapter not admitted: %s %s", state, reason)
			}
			for _, field := range []string{"manifest", "files", "platform", "sha", "integrity", "startup", "protocol", "unknown"} {
				bad, b := good, binding
				switch field {
				case "manifest":
					bad.Manifest = nil
				case "files":
					bad.FilesState = "missing"
				case "platform":
					bad.Platform = "mismatch"
				case "sha":
					bad.ManifestSHA256 = strings.Repeat("b", 64)
				case "integrity":
					bad.Integrity = "not_checked"
				case "startup":
					bad.StartupTest = "not_tested"
				case "protocol":
					bad.Manifest = &components.Manifest{Protocols: []string{"unknown/v1"}}
				case "unknown":
					b.Component.Protocol = "unknown/v1"
					bad.Manifest = &components.Manifest{ComponentID: tc.id, Version: "1", Protocols: []string{"unknown/v1"}}
				}
				if state, _ := componentCapability(b, []components.Evidence{bad}); state == integration.CapabilityAvailable {
					t.Fatalf("admitted %s evidence", field)
				}
			}
		})
	}
}

func TestComponentCapabilityRejectsUnimplementedContracts(t *testing.T) {
	for _, tc := range []struct{ scene, id, protocol string }{
		{"timbre.separate", "demucs-runner", "unknown/v1"},
		{"timbre.convert", "seedvc-runner", "unknown/v1"},
		{"highlight.score", "llama-runtime", "fengshen.llama/v1"},
		{"highlight.transcribe", "whisper-stream", "fengshen.whispercpp.batch/v1"},
		{"unknown.scene", "haar-consumer", "haar/v1"},
	} {
		binding := bindings.Binding{Scene: tc.scene, Component: integration.ExecutionComponentRef{ID: tc.id, Version: "1", ManifestSHA256: strings.Repeat("a", 64), Protocol: tc.protocol}}
		evidence := components.Evidence{ComponentID: tc.id, Version: "1", ManifestSHA256: binding.Component.ManifestSHA256, Manifest: &components.Manifest{Protocols: []string{tc.protocol}}, FilesState: "complete", Platform: "matched", Integrity: "matched", StartupTest: "passed"}
		if state, reason := componentCapability(binding, []components.Evidence{evidence}); state != integration.CapabilityIncompatible || reason != "center_adapter_missing" {
			t.Fatalf("unsupported %s %s %s: %s %s", tc.scene, tc.id, tc.protocol, state, reason)
		}
	}
}

func TestTimbreComponentCapabilityFixedCLIAdmission(t *testing.T) {
	for _, tc := range []struct{ scene, id, protocol string }{
		{"timbre.separate", "demucs-runner", "fengshen.demucs.cli/v1"},
		{"timbre.convert", "seedvc-runner", "fengshen.seedvc.cli/v1"},
	} {
		t.Run(tc.scene, func(t *testing.T) {
			binding := bindings.Binding{Scene: tc.scene, Component: integration.ExecutionComponentRef{ID: tc.id, Version: "1", ManifestSHA256: strings.Repeat("a", 64), Protocol: tc.protocol}}
			good := components.Evidence{ComponentID: tc.id, Version: "1", ManifestSHA256: binding.Component.ManifestSHA256, Manifest: &components.Manifest{ComponentID: tc.id, Version: "1", Protocols: []string{tc.protocol}}, FilesState: "complete", Platform: "matched", Integrity: "matched", StartupTest: "passed", Inference: "not_tested"}
			if state, reason := componentCapability(binding, []components.Evidence{good}); state != integration.CapabilityAvailable || reason != "" {
				t.Fatalf("fixed CLI contract rejected: %s %s", state, reason)
			}
			for _, mutation := range []string{"manifest", "files", "platform", "sha", "integrity", "startup", "legacy protocols", "empty protocol", "unknown protocol"} {
				bad, b := good, binding
				switch mutation {
				case "manifest":
					bad.Manifest = nil
				case "files":
					bad.FilesState = "missing"
				case "platform":
					bad.Platform = "mismatch"
				case "sha":
					bad.ManifestSHA256 = strings.Repeat("b", 64)
				case "integrity":
					bad.Integrity = "not_checked"
				case "startup":
					bad.StartupTest = "not_tested"
				case "legacy protocols":
					bad.Manifest = &components.Manifest{Protocols: []string{}}
				case "empty protocol":
					b.Component.Protocol = ""
					bad.Manifest = &components.Manifest{Protocols: []string{""}}
				case "unknown protocol":
					b.Component.Protocol = "unknown/v1"
					bad.Manifest = &components.Manifest{Protocols: []string{"unknown/v1"}}
				}
				if state, _ := componentCapability(b, []components.Evidence{bad}); state == integration.CapabilityAvailable {
					t.Fatalf("admitted %s", mutation)
				}
			}
			if good.Inference != "not_tested" {
				t.Fatal("contract admission invented inference evidence")
			}
		})
	}
}

func TestTimbreComponentAdapterExactAllowlist(t *testing.T) {
	for _, scene := range []string{"timbre.separate", "timbre.convert", "highlight.transcribe", "highlight.score", "highlight.vision", "image.face", "unknown.scene"} {
		for _, id := range []string{"demucs-runner", "seedvc-runner", "unknown-runner"} {
			for _, protocol := range []string{"", "unknown/v1", "fengshen.demucs.cli/v0", "fengshen.seedvc.cli/v0", "fengshen.demucs.cli/v1", "fengshen.seedvc.cli/v1", "FENGSHEN.DEMUCS.CLI/V1", "fengshen.seedvc.cli/v1 "} {
				want := scene == "timbre.separate" && id == "demucs-runner" && protocol == "fengshen.demucs.cli/v1" || scene == "timbre.convert" && id == "seedvc-runner" && protocol == "fengshen.seedvc.cli/v1"
				binding := bindings.Binding{Scene: scene, Component: integration.ExecutionComponentRef{ID: id, Protocol: protocol}}
				if got := componentAdapterSupported(binding); got != want {
					t.Errorf("allowlist %s %s %q = %v, want %v", scene, id, protocol, got, want)
				}
			}
		}
	}
}

func TestBindingRejectsStaleAndUnrecognized(t *testing.T) {
	m := fixtureManager(t)
	path := filepath.Join(t.TempDir(), "unknown.gguf")
	put(t, path, "unknown")
	if _, err := m.Registration(context.Background(), "local-text", path, 0, false); err != nil {
		t.Fatal(err)
	}
	request := bindingSelection(t, m, "highlight.score", "llama-runtime", "local-text")
	m.mu.Lock()
	if _, err := m.resolveBindingLocked(context.Background(), "highlight.score", request); err == nil {
		t.Fatal("unrecognized registration became executable")
	}
	m.mu.Unlock()
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "small")
	request = bindingSelection(t, m, "highlight.transcribe", "whisper-batch", "whisper-asr")
	put(t, filepath.Join(m.root, "models", "ggml-small.bin"), "changed-size")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.resolveBindingLocked(context.Background(), "highlight.transcribe", request); err == nil {
		t.Fatal("changed identity/metadata accepted")
	}
	m.inventories[request.InventoryID].Stale = true
	if _, err := m.resolveBindingLocked(context.Background(), "highlight.transcribe", request); err == nil {
		t.Fatal("stale inventory accepted")
	}
}
