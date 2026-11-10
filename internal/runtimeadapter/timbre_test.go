package runtimeadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

// Synthetic PCM is a transport/contract fixture, not model inference evidence.
func timbreTestWAV(rate int, channels int, seconds int) []byte {
	n := rate * channels * 2 * seconds
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	return b
}
func timbreTestExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := runtimeassets.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return newShimTestExecutor(t, s)
}
func timbreTestReq(scene string) runtimejobs.Request {
	id := "demucs-runner"
	if scene == "timbre.convert" {
		id = "seedvc-runner"
	}
	return runtimejobs.Request{TaskID: "task", SessionID: "session", Binding: integration.ExecutionBindingRef{Scene: scene}, Body: runtimejobs.Body{Component: integration.ExecutionComponentRef{ID: id, Version: "v1"}}}
}
func TestTimbrePlanCompleteBindings(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		id, model string
		sigs      []string
	}{{"demucs", "htdemucs", []string{"955717e8"}}, {"demucs-ft", "htdemucs_ft", []string{"f7e0c4bc", "d12395a8", "92cfc3b6", "04573f0d"}}} {
		for _, format := range []string{"th", "hf-safetensors"} {
			t.Run(tc.id+format, func(t *testing.T) {
				r := timbreBagFixture(t, tc.model)
				r.Binding = bindings.Binding{Device: "cpu"}
				for _, sig := range tc.sigs {
					ext := ".safetensors"
					if format == "th" {
						ext = "-01234567.th"
					}
					r.Models = append(r.Models, ModelFile{tc.id, filepath.Join(root, sig+ext)})
				}
				args, err := timbrePlan(r, false)
				if err != nil {
					t.Fatal(err)
				}
				joined := strings.Join(args, "|")
				if !strings.Contains(joined, "--model|"+tc.model) || !strings.Contains(joined, "--weight-format|"+format) || strings.Count(joined, "--weight|") != len(tc.sigs) || !strings.Contains(joined, "--bag|"+filepath.Join(r.ComponentRoot, "resources", "demucs", tc.model+".yaml")) {
					t.Fatal(args)
				}
				r.Models = r.Models[:len(r.Models)-1]
				if _, err = timbrePlan(r, false); err == nil {
					t.Fatal("incomplete group accepted")
				}
			})
		}
	}
	r := timbreBagFixture(t, "htdemucs")
	r.Binding.Device = "cpu"
	r.Models = []ModelFile{{"demucs", filepath.Join(root, "wrong.th")}}
	if _, err := timbrePlan(r, false); err == nil {
		t.Fatal("wrong signature accepted")
	}
	r = timbreBagFixture(t, "htdemucs_ft")
	r.Binding.Device = "cpu"
	for i, sig := range []string{"f7e0c4bc", "d12395a8", "92cfc3b6", "04573f0d"} {
		dir := root
		if i == 3 {
			dir = filepath.Join(root, "other")
		}
		r.Models = append(r.Models, ModelFile{"demucs-ft", filepath.Join(dir, sig+".th")})
	}
	if _, err := timbrePlan(r, false); err == nil {
		t.Fatal("cross repo accepted")
	}
	r.Models[3].Path = filepath.Join(root, "04573f0d.safetensors")
	if _, err := timbrePlan(r, false); err == nil {
		t.Fatal("mixed formats accepted")
	}
}
func timbreBagFixture(t *testing.T, model string) Resolved {
	t.Helper()
	root := t.TempDir()
	rel := "resources/demucs/" + model + ".yaml"
	data := []byte("models: ['955717e8']\n")
	if model == "htdemucs_ft" {
		data = []byte("models: ['f7e0c4bc', 'd12395a8', '92cfc3b6', '04573f0d']\nweights: [[1.,0.,0.,0.],[0.,1.,0.,0.],[0.,0.,1.,0.],[0.,0.,0.,1.]]\n")
	}
	if err := os.MkdirAll(filepath.Join(root, "resources", "demucs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), data, 0600); err != nil {
		t.Fatal(err)
	}
	return Resolved{ComponentRoot: root, Manifest: components.Manifest{ComponentID: "demucs-runner", Files: []components.File{{Path: rel, Bytes: fmt.Sprint(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}}}
}

func TestTimbreBagMustBeManifestResource(t *testing.T) {
	for _, mutation := range []string{"missing manifest", "wrong path", "wrong hash", "wrong size", "missing file", "modified", "duplicate", "model yaml", "relative root", "linked"} {
		t.Run(mutation, func(t *testing.T) {
			r := timbreBagFixture(t, "htdemucs")
			r.Binding.Device = "cpu"
			repo := t.TempDir()
			r.Models = []ModelFile{{"demucs", filepath.Join(repo, "955717e8.th")}}
			bag := filepath.Join(r.ComponentRoot, "resources", "demucs", "htdemucs.yaml")
			// A neighboring YAML must never rescue a missing component resource.
			if err := os.WriteFile(filepath.Join(repo, "htdemucs.yaml"), []byte("models: ['955717e8']\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing manifest":
				r.Manifest.Files = nil
			case "wrong path":
				r.Manifest.Files[0].Path = "htdemucs.yaml"
			case "wrong hash":
				r.Manifest.Files[0].SHA256 = strings.Repeat("0", 64)
			case "wrong size":
				r.Manifest.Files[0].Bytes = "1"
			case "missing file":
				if err := os.Remove(bag); err != nil {
					t.Fatal(err)
				}
			case "modified":
				if err := os.WriteFile(bag, []byte("models: ['wrong']\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				r.Manifest.Files = append(r.Manifest.Files, r.Manifest.Files[0])
			case "model yaml":
				r.Models = append(r.Models, ModelFile{"demucs", filepath.Join(repo, "htdemucs.yaml")})
			case "relative root":
				r.ComponentRoot = "."
			case "linked":
				data, err := os.ReadFile(bag)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "bag.yaml")
				if err = os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(bag); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, bag); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			if _, err := timbrePlan(r, false); err == nil {
				t.Fatal("untrusted bag accepted")
			}
		})
	}
}

func TestTimbreSeedDependencies(t *testing.T) {
	root := t.TempDir()
	r := Resolved{Binding: bindings.Binding{Device: "cpu"}}
	for id, names := range map[string][]string{"seed-vc": {"seed.pth", "config.yml"}, "seed-whisper": {"config.json", "preprocessor_config.json", "model.safetensors"}, "campplus": {"campplus_cn_common.bin"}, "bigvgan": {"config.json", "bigvgan_generator.pt"}} {
		for _, name := range names {
			r.Models = append(r.Models, ModelFile{id, filepath.Join(root, id, name)})
		}
	}
	for _, device := range []string{"cpu", "cuda", "", "auto", "mps", "CUDA", "cuda:0", "cpu "} {
		t.Run("device="+device, func(t *testing.T) {
			bound := r
			bound.Binding.Device = device
			args, err := timbrePlan(bound, true)
			if device != "cpu" && device != "cuda" {
				if !errors.Is(err, errTimbreBinding) || args != nil {
					t.Fatalf("invalid device accepted: %q %v %v", device, args, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, "|")
			if strings.Count(joined, "--device|") != 1 || !strings.HasSuffix(joined, "--device|"+device) || !strings.Contains(joined, "--steps|25|--pitch|0|") {
				t.Fatalf("bound device or fixed options missing: %v", args)
			}
		})
	}
	args, err := timbrePlan(r, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"--checkpoint", "--config", "--seed-whisper", "--campplus", "--bigvgan", "--pitch"} {
		if !strings.Contains(strings.Join(args, "|"), option+"|") {
			t.Fatal(args)
		}
	}
	for i := range r.Models {
		bad := r
		bad.Models = append([]ModelFile(nil), r.Models[:i]...)
		bad.Models = append(bad.Models, r.Models[i+1:]...)
		if _, err := timbrePlan(bad, true); err == nil {
			t.Fatalf("missing dependency %d accepted", i)
		}
	}
	r.Models = append(r.Models, ModelFile{"seed-whisper", filepath.Join(root, "seed-whisper", "pytorch_model.bin")})
	if _, err := timbrePlan(r, true); err == nil {
		t.Fatal("ambiguous whisper format accepted")
	}
}
func TestTimbreControlledOutputImport(t *testing.T) {
	e := timbreTestExecutor(t)
	req := timbreTestReq("timbre.separate")
	dir := t.TempDir()
	data := timbreTestWAV(44100, 2, 1)
	for _, name := range []string{"vocals.wav", "accompaniment.wav"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.timbreOutputs(context.Background(), req, dir, []string{"vocals.wav", "accompaniment.wav"}, 44100, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Media) != 2 || got.Media[0].ID == got.Media[1].ID {
		t.Fatal(got)
	}
	for _, media := range got.Media {
		if media.DurationMS != 1000 || media.SampleRate != 44100 || media.Channels != 2 || len(media.SHA256) != 64 {
			t.Fatal(media)
		}
		reader, meta, err := e.assets.Download(context.Background(), req.TaskID, media.ID)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, err = b.ReadFrom(reader)
		reader.Close()
		if err != nil || !bytes.Equal(data, b.Bytes()) || meta.SHA256 != media.SHA256 {
			t.Fatal("import changed bytes", err)
		}
		if _, _, err = e.assets.Download(context.Background(), "other", media.ID); !errors.Is(err, runtimeassets.ErrForbidden) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = e.timbreOutputs(ctx, req, dir, []string{"vocals.wav"}, 44100, 2, 1)
	if !errors.Is(err, context.Canceled) || len(got.Media) != 0 {
		t.Fatalf("cancel %+v %v", got, err)
	}
}
func TestTimbreRejectOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{{"wrong rate", timbreTestWAV(22050, 2, 1)}, {"wrong channels", timbreTestWAV(44100, 1, 1)}, {"wrong duration", timbreTestWAV(44100, 2, 2)}, {"not WAV", []byte("OK C:/model/checkpoint")}, {"truncated", timbreTestWAV(44100, 2, 1)[:100]}} {
		t.Run(tc.name, func(t *testing.T) {
			e := timbreTestExecutor(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "vocals.wav"), tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := e.timbreOutputs(context.Background(), timbreTestReq("timbre.separate"), dir, []string{"vocals.wav"}, 44100, 2, 1)
			if err == nil || len(got.Media) != 0 {
				t.Fatalf("accepted %+v %v", got, err)
			}
		})
	}
	e := timbreTestExecutor(t)
	if _, err := e.timbreOutputs(context.Background(), timbreTestReq("timbre.separate"), t.TempDir(), []string{"../model.wav"}, 44100, 2, 1); err == nil {
		t.Fatal("path accepted")
	}
}
func TestTimbreRejectLinkedOutput(t *testing.T) {
	e := timbreTestExecutor(t)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "model.wav")
	if err := os.WriteFile(target, timbreTestWAV(44100, 2, 1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "vocals.wav")); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	if _, err := e.timbreOutputs(context.Background(), timbreTestReq("timbre.separate"), dir, []string{"vocals.wav"}, 44100, 2, 1); err == nil {
		t.Fatal("linked output accepted")
	}
}
func TestTimbreInputCopyAndTaskScope(t *testing.T) {
	e := timbreTestExecutor(t)
	ctx := context.Background()
	req := timbreTestReq("timbre.separate")
	data := timbreTestWAV(16000, 1, 1)
	a, err := e.assets.Upload(ctx, req.TaskID, req.SessionID, runtimeassets.WAV, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m, err := e.timbreInput(ctx, req, a.ID(), dir, "source.wav")
	if err != nil || m.SampleRate != 16000 {
		t.Fatal(m, err)
	}
	copied, err := os.ReadFile(filepath.Join(dir, "source.wav"))
	if err != nil || !bytes.Equal(copied, data) {
		t.Fatal("copy mismatch", err)
	}
	if _, err = e.timbreInput(ctx, req, a.ID(), dir, "source.wav"); err == nil {
		t.Fatal("overwrote input")
	}
	req.TaskID = "other"
	if _, err = e.timbreInput(ctx, req, a.ID(), t.TempDir(), "source.wav"); !errors.Is(err, runtimeassets.ErrForbidden) {
		t.Fatal(err)
	}
}
func TestTimbreConversionOutputContract(t *testing.T) {
	e := timbreTestExecutor(t)
	req := timbreTestReq("timbre.convert")
	dir := t.TempDir()
	data := timbreTestWAV(22050, 1, 1)
	if err := os.WriteFile(filepath.Join(dir, "converted.wav"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := e.timbreOutputs(context.Background(), req, dir, []string{"converted.wav"}, 22050, 1, 1)
	if err != nil || len(got.Media) != 1 || got.Media[0].SampleRate != 22050 || got.Media[0].Channels != 1 || got.Media[0].DurationMS != 1000 {
		t.Fatalf("conversion fixture %+v %v", got, err)
	}
}

func TestTimbreFixedCLIProtocolGate(t *testing.T) {
	for _, tc := range []struct{ scene, id, protocol, other string }{
		{"timbre.separate", "demucs-runner", "fengshen.demucs.cli/v1", "fengshen.seedvc.cli/v1"},
		{"timbre.convert", "seedvc-runner", "fengshen.seedvc.cli/v1", "fengshen.demucs.cli/v1"},
	} {
		t.Run(tc.scene, func(t *testing.T) {
			for _, mutation := range []string{"valid", "request empty", "request unknown", "request old", "request case", "request whitespace", "request cross", "binding empty", "binding unknown", "binding cross", "both unknown", "binding id", "binding version", "binding hash", "manifest nil", "manifest empty", "manifest unknown", "manifest old", "manifest case", "manifest whitespace", "manifest cross", "manifest unrelated first"} {
				t.Run(mutation, func(t *testing.T) {
					req := timbreTestReq(tc.scene)
					req.Body.Component.Protocol = tc.protocol
					req.Body.Component.ManifestSHA256 = strings.Repeat("a", 64)
					req.Body.Separate = &runtimejobs.SeparateInput{AssetID: "never-opened"}
					req.Body.Convert = &runtimejobs.ConvertInput{AssetID: "never-opened", ReferenceAssetID: "never-opened"}
					r := Resolved{
						Binding:  bindings.Binding{Scene: tc.scene, Component: req.Body.Component, Device: "cuda"},
						Manifest: components.Manifest{Schema: 1, ComponentID: tc.id, Version: "v1", OS: "windows", Arch: "amd64", EntryType: "executable", Entry: tc.id + ".exe", Packaging: "pyinstaller-onedir", Protocols: []string{tc.protocol}, Files: []components.File{{Path: tc.id + ".exe", Bytes: "1", SHA256: strings.Repeat("b", 64)}}},
					}
					switch mutation {
					case "request empty":
						req.Body.Component.Protocol = ""
					case "request unknown":
						req.Body.Component.Protocol = "unknown/v1"
					case "request old":
						req.Body.Component.Protocol = strings.TrimSuffix(tc.protocol, "1") + "0"
					case "request case":
						req.Body.Component.Protocol = strings.ToUpper(tc.protocol)
					case "request whitespace":
						req.Body.Component.Protocol = tc.protocol + " "
					case "request cross":
						req.Body.Component.Protocol = tc.other
					case "binding empty":
						r.Binding.Component.Protocol = ""
					case "binding unknown":
						r.Binding.Component.Protocol = "unknown/v1"
					case "binding cross":
						r.Binding.Component.Protocol = tc.other
					case "both unknown":
						req.Body.Component.Protocol = "unknown/v1"
						r.Binding.Component.Protocol = "unknown/v1"
						r.Manifest.Protocols = []string{"unknown/v1"}
					case "binding id":
						r.Binding.Component.ID = "unknown-runner"
					case "binding version":
						r.Binding.Component.Version = "v0"
					case "binding hash":
						r.Binding.Component.ManifestSHA256 = strings.Repeat("c", 64)
					case "manifest nil":
						r.Manifest.Protocols = nil
					case "manifest empty":
						r.Manifest.Protocols = []string{}
					case "manifest unknown":
						r.Manifest.Protocols = []string{"unknown/v1"}
					case "manifest old":
						r.Manifest.Protocols = []string{strings.TrimSuffix(tc.protocol, "1") + "0"}
					case "manifest case":
						r.Manifest.Protocols = []string{strings.ToUpper(tc.protocol)}
					case "manifest whitespace":
						r.Manifest.Protocols = []string{tc.protocol + " "}
					case "manifest cross":
						r.Manifest.Protocols = []string{tc.other}
					case "manifest unrelated first":
						r.Manifest.Protocols = []string{"unrelated/v1", tc.protocol}
					}
					if err := components.Validate(r.Manifest); err != nil {
						t.Fatal("invalid test manifest:", err)
					}
					// No model files: a valid protocol reaches planning, never asset I/O or launch.
					e := &Executor{}
					got, err := e.timbre(context.Background(), req, r, func(runtimejobs.Progress) error {
						t.Fatal("protocol test reached execution")
						return nil
					})
					if mutation == "valid" || mutation == "manifest unrelated first" {
						if !errors.Is(err, errTimbreBinding) {
							t.Fatalf("valid contract did not reach planning: %v", err)
						}
					} else if err == nil || err.Error() != "center_protocol_unsupported" {
						t.Fatalf("incompatible contract reached planning: %v", err)
					}
					if len(got.Media) != 0 {
						t.Fatal("protocol test published media")
					}
				})
			}
		})
	}
}

func TestTimbreRequiresFrozenManifest(t *testing.T) {
	e := timbreTestExecutor(t)
	req := timbreTestReq("timbre.separate")
	req.Body.Separate = &runtimejobs.SeparateInput{AssetID: "never-opened"}
	for _, packaging := range []string{"", "native", "go", "python"} {
		r := Resolved{Manifest: components.Manifest{ComponentID: "demucs-runner", Version: "v1", EntryType: "executable", Packaging: packaging}}
		got, err := e.timbre(context.Background(), req, r, nil)
		if err == nil || len(got.Media) != 0 {
			t.Fatalf("accepted %s", packaging)
		}
	}
}
