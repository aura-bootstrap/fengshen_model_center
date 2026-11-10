package main

import (
	"path/filepath"
	"strings"
)

type Model struct {
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	Family                string     `json:"family"`
	Category              string     `json:"category"`
	Purpose               string     `json:"purpose"`
	Invocation            string     `json:"invocation"`
	Runtime               string     `json:"runtime"`
	Optional              bool       `json:"optional"`
	Patterns              []string   `json:"-"`
	Description           string     `json:"description"`
	Dependencies          []string   `json:"dependencies"`
	Components            []string   `json:"components"`
	ComponentAlternatives [][]string `json:"component_alternatives,omitempty"`
	AllowedExtensions     []string   `json:"allowed_extensions"`
	RecognitionRules      []string   `json:"recognition_rules"`
	Consumer              string     `json:"consumer"`
	IdentityNote          string     `json:"identity_note"`
}

func catalog() []Model {
	models := []Model{
		{ID: "whisper-asr", Name: "Whisper · 高光转写", Family: "Whisper", Category: "语音识别", Purpose: "将台词转为带时间戳的文本，供高光分析使用。", Invocation: "Go → whisper.cpp", Runtime: "原生推理", Optional: true, Description: "型号由外置 GGML 模型决定；与音色转换的 Whisper Small 权重不通用。"},
		{ID: "whisper-vad", Name: "Whisper VAD", Family: "VAD", Category: "语音识别", Purpose: "检测说话区间，辅助 Whisper 转写。", Invocation: "Go → whisper.cpp", Runtime: "原生推理", Optional: true, Description: "可选外置权重，未绑定具体型号；不包含无模型的能量 VAD 算法。"},
		{ID: "local-text", Name: "本地文本大模型", Family: "可配置 LLM", Category: "高光分析", Purpose: "理解台词、评选高光并判断内容完整性。", Invocation: "Go → 本地 HTTP API", Runtime: "本地模型服务", Optional: true, Description: "模型名称由客户端配置指定；管理中心不读取密钥，也不启动推理服务。"},
		{ID: "local-vision", Name: "本地视觉大模型", Family: "可配置 VLM", Category: "高光分析", Purpose: "为高光候选补充画面理解和视觉审查。", Invocation: "Go → 本地 HTTP API", Runtime: "本地模型服务", Optional: true, Description: "客户端默认关闭；型号和权重路径需要单独登记。"},
		{ID: "demucs", Name: "Demucs · htdemucs", Family: "Demucs", Category: "音色处理", Purpose: "将音轨分离为人声与伴奏。", Invocation: "Go → Python / PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"955717e8-*.th"}, Description: "默认分离模型；缺失时只报告未发现，不自动下载。"},
		{ID: "demucs-ft", Name: "Demucs · htdemucs_ft", Family: "Demucs", Category: "音色处理", Purpose: "通过四个微调子模型进行音源分离。", Invocation: "Go → Python / PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"f7e0c4bc-*.th", "d12395a8-*.th", "92cfc3b6-*.th", "04573f0d-*.th"}, Description: "四份权重齐全才显示已发现；体积按全部子模型合计。"},
		{ID: "seed-vc", Name: "Seed-VC", Family: "Seed-VC", Category: "音色处理", Purpose: "以参考声音为条件，转换原有人声音色。", Invocation: "Go → Python / PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth"}, Description: "当前产品使用非 F0 条件分支；需要配套的 Whisper、CAMPPlus 和 BigVGAN。"},
		{ID: "seed-whisper", Name: "Whisper Small · 音色特征", Family: "Whisper", Category: "音色处理", Purpose: "为 Seed-VC 提取语音内容特征，不负责字幕转写。", Invocation: "Python → Transformers / PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"model.safetensors"}, Description: "仅匹配 openai/whisper-small 缓存，避免将其他 safetensors 误判为 Whisper。"},
		{ID: "campplus", Name: "CAMPPlus", Family: "CAMPPlus", Category: "音色处理", Purpose: "提取参考音频的说话人和音色特征。", Invocation: "Python → PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"campplus_cn_common.bin"}, Description: "Seed-VC 音色编码子模型。"},
		{ID: "bigvgan", Name: "BigVGAN · 22 kHz", Family: "BigVGAN", Category: "音色处理", Purpose: "将转换后的声学特征还原为音频波形。", Invocation: "Python → PyTorch", Runtime: "Python", Optional: true, Patterns: []string{"bigvgan_generator.pt"}, Description: "nvidia/bigvgan_v2_22khz_80band_256x；当前推理不要求自定义 CUDA kernel。"},
		{ID: "haar-frontal", Name: "Haar · 正脸检测", Family: "OpenCV Haar", Category: "人脸检测", Purpose: "检测人物参考图是否包含正脸。", Invocation: "Go 直接检测", Runtime: "Go", Patterns: []string{"haarcascade_frontalface_default.xml"}, Description: "OpenCV 格式模型，由 shim 读取模型清单绑定的 XML；缺失时只报告未发现。"},
		{ID: "haar-profile", Name: "Haar · 侧脸检测", Family: "OpenCV Haar", Category: "人脸检测", Purpose: "辅助区分侧脸与没有人脸。", Invocation: "Go 直接检测", Runtime: "Go", Patterns: []string{"haarcascade_profileface.xml"}, Description: "OpenCV 格式模型，由 shim 读取模型清单绑定的 XML；缺失时只报告未发现。"},
	}
	for i := range models {
		m := &models[i]
		m.Dependencies = []string{}
		m.Components = append([]string{}, m.Patterns...)
		m.RecognitionRules = append([]string{}, m.Patterns...)
		m.IdentityNote = "型号未确认；规则匹配不是内容认证或推理验证。"
		switch m.ID {
		case "whisper-asr":
			m.AllowedExtensions = []string{".bin", ".ggml", ".gguf"}
			m.Consumer = "whisper-batch / whisper-stream"
			m.ComponentAlternatives = [][]string{{"ggml-small.bin"}, {"ggml-large-v3-turbo.bin"}}
			m.Description += " 支持按精确文件名识别 small 和 large-v3-turbo 的 GGML 资产；任一非空单文件组成独立一套，不证明文件内容或 CLI 兼容性。"
		case "whisper-vad":
			m.AllowedExtensions = []string{".bin", ".ggml", ".onnx"}
			m.Consumer = "whisper-batch / whisper-stream"
			m.ComponentAlternatives = [][]string{{"ggml-silero-v5.1.2.bin"}}
			m.Description += " 支持按精确文件名识别 whisper.cpp 的 Silero v5.1.2 GGML 资产，不将普通 ONNX 自动认定为兼容。"
		case "local-text", "local-vision":
			m.AllowedExtensions = []string{".gguf", ".safetensors", ".bin"}
			m.Consumer = "独立 HTTP 服务（本版本接入入口已隐藏，服务未检测）"
			if m.ID == "local-text" {
				m.ComponentAlternatives = [][]string{{"qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf", "qwen2.5-7b-instruct-q4_k_m-00002-of-00002.gguf"}}
				m.Description += " 支持 Qwen2.5-7B-Instruct Q4_K_M 的精确两分片文件名，同一目录两份均非空才组成齐全；服务仍未检测。"
			} else {
				m.ComponentAlternatives = [][]string{
					{"SmolVLM-500M-Instruct-Q8_0.gguf", "mmproj-SmolVLM-500M-Instruct-f16.gguf"},
					{"Qwen3-VL-2B-Instruct-Q8_0.gguf", "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf"},
				}
				m.Description += " 支持 ggml-org/SmolVLM-500M-Instruct-GGUF 和 ggml-org/Qwen3-VL-2B-Instruct-GGUF 仓库 snapshot 内各自精确主模型与 projector 文件名，须同一修订目录两份均非空；仅资产识别，服务及画面理解能力未验证。"
			}
		case "demucs", "demucs-ft":
			m.AllowedExtensions = []string{".th", ".safetensors"}
			m.Consumer = "demucs-runner"
			repo, parts := demucsHFComponents(*m)
			for _, part := range parts {
				m.RecognitionRules = append(m.RecognitionRules, repo+"/snapshots/{revision}/"+part)
			}
			m.Description += " 支持旧版 .th 或对应 Hugging Face 仓库的 .safetensors；每套组成须在同一目录中以同一种格式齐全。"
		case "seed-vc":
			m.AllowedExtensions = []string{".pth"}
			m.Consumer = "seedvc-runner"
			m.Dependencies = []string{"seed-whisper", "campplus", "bigvgan"}
		case "seed-whisper":
			m.AllowedExtensions = []string{".safetensors"}
			m.Consumer = "seedvc-runner"
			m.RecognitionRules = append(m.RecognitionRules, "models--openai--whisper-small/snapshots/{revision}")
		case "campplus":
			m.AllowedExtensions = []string{".bin"}
			m.Consumer = "seedvc-runner"
		case "bigvgan":
			m.AllowedExtensions = []string{".pt"}
			m.Consumer = "seedvc-runner"
			m.RecognitionRules = append(m.RecognitionRules, "models--nvidia--bigvgan_v2_22khz_80band_256x/snapshots/{revision}")
		default:
			m.AllowedExtensions = []string{".xml"}
			m.Consumer = "haar-consumer"
		}
		for _, parts := range m.ComponentAlternatives {
			for _, part := range parts {
				m.Patterns = append(m.Patterns, part)
				if m.ID == "local-vision" {
					m.RecognitionRules = append(m.RecognitionRules, visionRepository(part)+"/snapshots/{revision}/"+part)
				} else {
					m.RecognitionRules = append(m.RecognitionRules, part)
				}
			}
		}
	}
	return models
}

func findModel(id string) (Model, bool) {
	for _, m := range catalog() {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
func allowed(m Model, path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range m.AllowedExtensions {
		if ext == e {
			return true
		}
	}
	return false
}
func demucsHFComponents(m Model) (string, []string) {
	repo := ""
	switch m.ID {
	case "demucs":
		repo = "models--adefossez--HTDemucs"
	case "demucs-ft":
		repo = "models--adefossez--HTDemucs-ft"
	default:
		return "", nil
	}
	parts := make([]string, 0, len(m.Patterns))
	for _, pattern := range m.Patterns {
		parts = append(parts, strings.TrimSuffix(pattern, "-*.th")+".safetensors")
	}
	return repo, parts
}

func visionRepository(name string) string {
	switch name {
	case "SmolVLM-500M-Instruct-Q8_0.gguf", "mmproj-SmolVLM-500M-Instruct-f16.gguf":
		return "models--ggml-org--SmolVLM-500M-Instruct-GGUF"
	case "Qwen3-VL-2B-Instruct-Q8_0.gguf", "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf":
		return "models--ggml-org--Qwen3-VL-2B-Instruct-GGUF"
	default:
		return ""
	}
}

func recognizes(m Model, path string) bool {
	if m.ID == "local-vision" {
		revision := filepath.Dir(path)
		snapshots := filepath.Dir(revision)
		repo := visionRepository(filepath.Base(path))
		if repo == "" || filepath.Base(snapshots) != "snapshots" || !strings.EqualFold(filepath.Base(filepath.Dir(snapshots)), repo) {
			return false
		}
	}
	if repo, parts := demucsHFComponents(m); repo != "" && filepath.Ext(path) == ".safetensors" {
		revision := filepath.Dir(path)
		snapshots := filepath.Dir(revision)
		if filepath.Base(revision) == "." || filepath.Base(snapshots) != "snapshots" || !strings.EqualFold(filepath.Base(filepath.Dir(snapshots)), repo) {
			return false
		}
		for _, part := range parts {
			if filepath.Base(path) == part {
				return true
			}
		}
		return false
	}
	p := strings.ToLower(filepath.ToSlash(path))
	if m.ID == "seed-whisper" && !strings.Contains(p, "models--openai--whisper-small/snapshots/") {
		return false
	}
	if m.ID == "bigvgan" && !strings.Contains(p, "models--nvidia--bigvgan_v2_22khz_80band_256x/snapshots/") {
		return false
	}
	for _, pattern := range m.Patterns {
		if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
			return true
		}
	}
	return false
}
