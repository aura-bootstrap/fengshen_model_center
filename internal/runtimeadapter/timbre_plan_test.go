package runtimeadapter

import (
	"path/filepath"
	"strings"
)

// timbrePlan is retained only for binding-validation tests; the production path
// sends typed fields through timbreJobRequest and the shim owns the CLI template.
func timbrePlan(r Resolved, convert bool) ([]string, error) {
	groups := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range r.Models {
		if !filepath.IsAbs(f.Path) || seen[f.Path] {
			return nil, errTimbreBinding
		}
		seen[f.Path] = true
		groups[f.ModelID] = append(groups[f.ModelID], f.Path)
	}
	if convert {
		device := r.Binding.Device
		if device != "cpu" && device != "cuda" {
			return nil, errTimbreBinding
		}
		if len(groups) != 4 || len(groups["campplus"]) != 1 {
			return nil, errTimbreBinding
		}
		seed := groups["seed-vc"]
		if len(seed) != 2 {
			return nil, errTimbreBinding
		}
		var checkpoint, config string
		for _, p := range seed {
			switch strings.ToLower(filepath.Ext(p)) {
			case ".yml", ".yaml":
				config = p
			case ".pth", ".pt":
				checkpoint = p
			default:
				return nil, errTimbreBinding
			}
		}
		whisper, err := boundDirectory(groups["seed-whisper"], []string{"config.json", "preprocessor_config.json"}, []string{"model.safetensors", "pytorch_model.bin"})
		if err != nil {
			return nil, err
		}
		big, err := boundDirectory(groups["bigvgan"], []string{"config.json", "bigvgan_generator.pt"}, nil)
		if err != nil || checkpoint == "" || config == "" {
			return nil, errTimbreBinding
		}
		return []string{"--checkpoint", checkpoint, "--config", config, "--seed-whisper", whisper, "--campplus", groups["campplus"][0], "--bigvgan", big, "--steps", "25", "--pitch", "0", "--device", device}, nil
	}
	if len(groups) != 1 {
		return nil, errTimbreBinding
	}
	model, id := "htdemucs", "demucs"
	signatures := []string{"955717e8"}
	if len(groups["demucs-ft"]) != 0 {
		model, id = "htdemucs_ft", "demucs-ft"
		signatures = []string{"f7e0c4bc", "d12395a8", "92cfc3b6", "04573f0d"}
	}
	paths := groups[id]
	if len(paths) != len(signatures) {
		return nil, errTimbreBinding
	}
	bag, err := timbreBagResource(r, model)
	if err != nil {
		return nil, err
	}
	repo, format := "", ""
	weights := map[string]string{}
	for _, p := range paths {
		if repo == "" {
			repo = filepath.Dir(p)
		}
		if filepath.Dir(p) != repo {
			return nil, errTimbreBinding
		}
		base := filepath.Base(p)
		ext := filepath.Ext(p)
		sig := strings.TrimSuffix(base, ext)
		current := "hf-safetensors"
		if ext == ".th" {
			current = "th"
			sig = strings.SplitN(sig, "-", 2)[0]
		} else if ext != ".safetensors" {
			return nil, errTimbreBinding
		}
		if format != "" && current != format || weights[sig] != "" {
			return nil, errTimbreBinding
		}
		format = current
		weights[sig] = p
	}
	if bag == "" || len(weights) != len(signatures) {
		return nil, errTimbreBinding
	}
	device := r.Binding.Device
	if device != "cpu" && device != "cuda" {
		return nil, errTimbreBinding
	}
	args := []string{"--model", model, "--repo", repo, "--bag", bag, "--weight-format", format, "--device", device}
	for _, sig := range signatures {
		if weights[sig] == "" {
			return nil, errTimbreBinding
		}
		args = append(args, "--weight", weights[sig])
	}
	return args, nil
}
