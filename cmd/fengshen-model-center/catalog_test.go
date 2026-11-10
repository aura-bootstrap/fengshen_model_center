package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogContract(t *testing.T) {
	tests := []struct {
		id, category, runtime, invocation, consumer, purpose string
		optional                                             bool
		extensions, dependencies, components                 []string
	}{
		{"whisper-asr", "语音识别", "原生推理", "Go → whisper.cpp", "whisper-batch / whisper-stream", "带时间戳", true, []string{".bin", ".ggml", ".gguf"}, []string{}, []string{}},
		{"whisper-vad", "语音识别", "原生推理", "Go → whisper.cpp", "whisper-batch / whisper-stream", "说话区间", true, []string{".bin", ".ggml", ".onnx"}, []string{}, []string{}},
		{"local-text", "高光分析", "本地模型服务", "Go → 本地 HTTP API", "独立 HTTP 服务（本版本接入入口已隐藏，服务未检测）", "评选高光", true, []string{".gguf", ".safetensors", ".bin"}, []string{}, []string{}},
		{"local-vision", "高光分析", "本地模型服务", "Go → 本地 HTTP API", "独立 HTTP 服务（本版本接入入口已隐藏，服务未检测）", "画面理解", true, []string{".gguf", ".safetensors", ".bin"}, []string{}, []string{}},
		{"demucs", "音色处理", "Python", "Go → Python / PyTorch", "demucs-runner", "人声与伴奏", true, []string{".th", ".safetensors"}, []string{}, []string{"955717e8-*.th"}},
		{"demucs-ft", "音色处理", "Python", "Go → Python / PyTorch", "demucs-runner", "四个微调子模型", true, []string{".th", ".safetensors"}, []string{}, []string{"f7e0c4bc-*.th", "d12395a8-*.th", "92cfc3b6-*.th", "04573f0d-*.th"}},
		{"seed-vc", "音色处理", "Python", "Go → Python / PyTorch", "seedvc-runner", "转换原有人声音色", true, []string{".pth"}, []string{"seed-whisper", "campplus", "bigvgan"}, []string{"DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth"}},
		{"seed-whisper", "音色处理", "Python", "Python → Transformers / PyTorch", "seedvc-runner", "不负责字幕转写", true, []string{".safetensors"}, []string{}, []string{"model.safetensors"}},
		{"campplus", "音色处理", "Python", "Python → PyTorch", "seedvc-runner", "说话人和音色特征", true, []string{".bin"}, []string{}, []string{"campplus_cn_common.bin"}},
		{"bigvgan", "音色处理", "Python", "Python → PyTorch", "seedvc-runner", "还原为音频波形", true, []string{".pt"}, []string{}, []string{"bigvgan_generator.pt"}},
		{"haar-frontal", "人脸检测", "Go", "Go 直接检测", "haar-consumer", "正脸", false, []string{".xml"}, []string{}, []string{"haarcascade_frontalface_default.xml"}},
		{"haar-profile", "人脸检测", "Go", "Go 直接检测", "haar-consumer", "侧脸", false, []string{".xml"}, []string{}, []string{"haarcascade_profileface.xml"}},
	}
	models := catalog()
	if len(models) != len(tests) {
		t.Fatalf("catalog=%d", len(models))
	}
	seen := map[string]bool{}
	for i, want := range tests {
		t.Run(want.id, func(t *testing.T) {
			m := models[i]
			if m.ID != want.id || seen[m.ID] || m.Name == "" || m.Family == "" || m.Description == "" {
				t.Fatalf("invalid catalog identity: %+v", m)
			}
			seen[m.ID] = true
			if m.Category != want.category || m.Runtime != want.runtime || m.Invocation != want.invocation || m.Consumer != want.consumer || m.Optional != want.optional || !strings.Contains(m.Purpose, want.purpose) {
				t.Fatalf("metadata differs: %+v", m)
			}
			if !reflect.DeepEqual(m.AllowedExtensions, want.extensions) || !reflect.DeepEqual(m.Dependencies, want.dependencies) || !reflect.DeepEqual(m.Components, want.components) {
				t.Fatalf("composition/extension contract differs: %+v", m)
			}
			if m.IdentityNote != "型号未确认；规则匹配不是内容认证或推理验证。" {
				t.Fatal(m.IdentityNote)
			}
			for _, extension := range want.extensions {
				if !allowed(m, "candidate"+strings.ToUpper(extension)) {
					t.Fatal("allowed extension rejected", extension)
				}
			}
			for _, extension := range []string{".py", ".cpp", ".exe", ".wav", ".mp3"} {
				if allowed(m, "candidate"+extension) {
					t.Fatal("non-model type admitted", extension)
				}
			}
			found, ok := findModel(want.id)
			if !ok || !reflect.DeepEqual(found, m) {
				t.Fatal("lookup differs from catalog")
			}
			if strings.HasPrefix(m.ID, "haar-") && (len(m.Components) != 1 || m.Components[0] != want.components[0]) {
				t.Fatal("Haar resource mismatch", m.Components)
			}
		})
	}
	for _, id := range []string{"energy-vad", "reference-audio", "python", "cloud", "unknown"} {
		if _, ok := findModel(id); ok {
			t.Fatal("non-model became catalog item", id)
		}
	}
	vad, _ := findModel("whisper-vad")
	if !strings.Contains(vad.Description, "能量 VAD") {
		t.Fatal("algorithm VAD boundary missing")
	}
}

func TestRecognitionRequiresRepository(t *testing.T) {
	m, _ := findModel("seed-whisper")
	if recognizes(m, "other/model.safetensors") {
		t.Fatal("unrelated safetensors recognized")
	}
	if !recognizes(m, "models--openai--whisper-small/snapshots/rev/model.safetensors") {
		t.Fatal("repository not recognized")
	}
	m, _ = findModel("haar-frontal")
	if recognizes(m, "arbitrary.xml") {
		t.Fatal("unrelated XML recognized")
	}
}

func TestDemucsHuggingFaceRecognition(t *testing.T) {
	for _, tc := range []struct{ id, repo, signature, legacy string }{
		{"demucs", "models--adefossez--HTDemucs", "955717e8", "955717e8-8726e21a.th"},
		{"demucs-ft", "models--adefossez--HTDemucs-ft", "f7e0c4bc", "f7e0c4bc-ba3fe64a.th"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			m, _ := findModel(tc.id)
			valid := filepath.Join("cache", tc.repo, "snapshots", "revision", tc.signature+".safetensors")
			if !recognizes(m, valid) || !recognizes(m, tc.legacy) {
				t.Fatal("supported model format not recognized")
			}
			for _, invalid := range []string{
				tc.signature + ".safetensors",
				filepath.Join("prefix-"+tc.repo, "snapshots", "revision", tc.signature+".safetensors"),
				filepath.Join(tc.repo+"-other", "snapshots", "revision", tc.signature+".safetensors"),
				filepath.Join(tc.repo, "snapshots", tc.signature+".safetensors"),
				filepath.Join(tc.repo, "snapshots", "revision", "nested", tc.signature+".safetensors"),
				filepath.Join(tc.repo, "blobs", tc.signature+".safetensors"),
				filepath.Join(tc.repo, "snapshots", "revision", "unknown.safetensors"),
				filepath.Join(tc.repo, "snapshots", "revision", tc.signature+"-copy.safetensors"),
			} {
				if recognizes(m, invalid) {
					t.Fatal("unrelated safetensors recognized", invalid)
				}
			}
			other, _ := findModel(map[string]string{"demucs": "demucs-ft", "demucs-ft": "demucs"}[tc.id])
			if recognizes(other, valid) {
				t.Fatal("repository crossed model boundary")
			}
		})
	}
}

func TestKnownStandaloneModelRecognition(t *testing.T) {
	for _, tc := range []struct{ id, name string }{
		{"whisper-asr", "ggml-small.bin"},
		{"whisper-asr", "ggml-large-v3-turbo.bin"},
		{"whisper-vad", "ggml-silero-v5.1.2.bin"},
		{"local-text", "qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf"},
		{"local-text", "qwen2.5-7b-instruct-q4_k_m-00002-of-00002.gguf"},
	} {
		m, _ := findModel(tc.id)
		if !recognizes(m, filepath.Join("weights", tc.name)) {
			t.Error("known filename not recognized", tc)
		}
		for _, name := range []string{"unknown.bin", "unknown.gguf", "copy-" + tc.name, tc.name + ".partial"} {
			if recognizes(m, filepath.Join("weights", name)) {
				t.Error("unconfirmed file acquired identity", tc.id, name)
			}
		}
		for _, id := range []string{"whisper-asr", "whisper-vad", "local-text", "local-vision"} {
			other, _ := findModel(id)
			if id != tc.id && recognizes(other, tc.name) {
				t.Error("model boundary crossed", id, tc.name)
			}
		}
	}
}

func TestSizeMetricJSONPreservesLargeBytesAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value SizeMetric
		bytes string
	}{
		{"unknown", metric(0, 0, false, false), `null`},
		{"zero", metric(0, 0, true, true), `"0"`},
		{"partial-large", metric(9007199254740993, 1, false, true), `"9007199254740993"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["bytes"]) != tc.bytes || string(fields["complete"]) != map[bool]string{true: "true", false: "false"}[tc.value.Complete] {
				t.Fatalf("lossy metric: %s", body)
			}
		})
	}
}
