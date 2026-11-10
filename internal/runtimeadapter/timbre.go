package runtimeadapter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

var errTimbreBinding = errors.New("center_model_incompatible")

// timbrePlan contains only resolved model paths and fixed runner options.
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

// Bags are immutable component resources, never model assets or adjacent YAMLs.
func timbreBagResource(r Resolved, model string) (string, error) {
	if r.Manifest.ComponentID != "demucs-runner" || !filepath.IsAbs(r.ComponentRoot) {
		return "", errTimbreBinding
	}
	rel := "resources/demucs/" + model + ".yaml"
	var pinned components.File
	count := 0
	for _, f := range r.Manifest.Files {
		if strings.EqualFold(f.Path, rel) {
			if f.Path != rel {
				return "", errTimbreBinding
			}
			pinned = f
			count++
		}
	}
	n, err := strconv.ParseInt(pinned.Bytes, 10, 64)
	digest, hashErr := hex.DecodeString(pinned.SHA256)
	if count != 1 || err != nil || n <= 0 || n > 64<<10 || hashErr != nil || len(digest) != sha256.Size {
		return "", errTimbreBinding
	}
	scope, err := safefs.Open(r.ComponentRoot)
	if err != nil {
		return "", errTimbreBinding
	}
	defer scope.Close()
	leaf, err := scope.Derive(filepath.FromSlash(rel))
	if err != nil {
		return "", errTimbreBinding
	}
	defer leaf.Close()
	f, entry, err := leaf.OpenFile(filepath.Base(rel))
	if err != nil {
		return "", errTimbreBinding
	}
	defer f.Close()
	h := sha256.New()
	read, err := io.Copy(h, io.LimitReader(f, n+1))
	if err != nil || entry.Size != n || read != n || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), pinned.SHA256) {
		return "", errTimbreBinding
	}
	return entry.Path, nil
}

// Directory loaders may access only these explicitly selected complete files.
func boundDirectory(paths, required, alternatives []string) (string, error) {
	if len(paths) != len(required)+min(1, len(alternatives)) {
		return "", errTimbreBinding
	}
	dir := ""
	names := map[string]bool{}
	for _, p := range paths {
		if dir == "" {
			dir = filepath.Dir(p)
		}
		if filepath.Dir(p) != dir || names[filepath.Base(p)] {
			return "", errTimbreBinding
		}
		names[filepath.Base(p)] = true
	}
	for _, n := range required {
		if !names[n] {
			return "", errTimbreBinding
		}
		delete(names, n)
	}
	if len(alternatives) > 0 {
		n := 0
		for _, name := range alternatives {
			if names[name] {
				n++
				delete(names, name)
			}
		}
		if n != 1 {
			return "", errTimbreBinding
		}
	}
	if len(names) != 0 {
		return "", errTimbreBinding
	}
	return dir, nil
}

type timbreContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r timbreContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

func (e *Executor) timbreInput(ctx context.Context, req runtimejobs.Request, id, dir, name string) (runtimeassets.Metadata, error) {
	src, meta, err := e.assets.Open(ctx, req.TaskID, id)
	if err != nil {
		return meta, err
	}
	defer src.Close()
	if meta.Kind != runtimeassets.WAV || meta.Channels < 1 || meta.Channels > 2 || meta.DurationSeconds <= 0 || meta.DurationSeconds > 86400 {
		return meta, runtimeassets.ErrInvalid
	}
	n, err := strconv.ParseInt(meta.Bytes, 10, 64)
	if err != nil || n <= 0 || n > 1<<30 {
		return meta, runtimeassets.ErrInvalid
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return meta, err
	}
	copied, copyErr := io.Copy(dst, io.LimitReader(timbreContextReader{ctx, src}, n+1))
	err = errors.Join(copyErr, dst.Close())
	if err != nil {
		return meta, err
	}
	if copied != n {
		return meta, runtimeassets.ErrInvalid
	}
	return meta, ctx.Err()
}

func (e *Executor) timbre(ctx context.Context, req runtimejobs.Request, r Resolved, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	empty := runtimejobs.Result{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	convert := req.Binding.Scene == "timbre.convert"
	component, protocol := "demucs-runner", integration.DemucsCLIProtocol
	if convert {
		component, protocol = "seedvc-runner", integration.SeedVCCLIProtocol
	}
	if (!convert && (req.Binding.Scene != "timbre.separate" || req.Body.Separate == nil)) || (convert && req.Body.Convert == nil) {
		return empty, errors.New("center_request_invalid")
	}
	if r.Manifest.ComponentID != component || r.Manifest.Packaging != "pyinstaller-onedir" || r.Manifest.EntryType != "executable" || components.Validate(r.Manifest) != nil || req.Body.Component.ID != component || r.Manifest.Version != req.Body.Component.Version {
		return empty, errors.New("center_component_missing")
	}
	supported := false
	for _, p := range r.Manifest.Protocols {
		if p == protocol {
			supported = true
		}
	}
	if !supported || req.Body.Component.Protocol != protocol || r.Binding.Component != req.Body.Component {
		return empty, errors.New("center_protocol_unsupported")
	}
	if convert && (req.Body.Convert.Pitch != 0 || r.Binding.Device != "cuda") {
		return empty, errTimbreBinding
	}
	args, err := timbrePlan(r, convert)
	if err != nil {
		return empty, err
	}
	dir, err := os.MkdirTemp(e.scratch, "timbre-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(dir)
	id := ""
	if convert {
		id = req.Body.Convert.AssetID
	} else {
		id = req.Body.Separate.AssetID
	}
	input, err := e.timbreInput(ctx, req, id, dir, "source.wav")
	if err != nil {
		return empty, err
	}
	outputs := []string{"vocals.wav", "accompaniment.wav"}
	rate, channels := uint32(44100), uint16(2)
	if convert {
		if _, err = e.timbreInput(ctx, req, req.Body.Convert.ReferenceAssetID, dir, "reference.wav"); err != nil {
			return empty, err
		}
		outputs = []string{"converted.wav"}
		rate, channels = 22050, 1
		args = append(args, "--source", filepath.Join(dir, "source.wav"), "--target", filepath.Join(dir, "reference.wav"), "--out", filepath.Join(dir, outputs[0]))
	} else {
		args = append(args, "--in", filepath.Join(dir, "source.wav"), "--outdir", dir)
	}
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning}); err != nil {
			return empty, err
		}
	}
	p, err := e.supervisor.Start(ctx, r.Entry, runtimeprocess.Options{Args: args, MaxStdinBytes: 1, MaxStdoutBytes: 4 << 20, MaxStderrBytes: 4 << 20, BufferBytes: 4 << 20})
	if err != nil {
		return empty, err
	}
	defer p.Close()
	_ = p.Stdin().Close()
	var wg sync.WaitGroup
	wg.Add(2)
	for _, stream := range []io.Reader{p.Stdout(), p.Stderr()} {
		go func(src io.Reader) { defer wg.Done(); _, _ = io.Copy(io.Discard, src) }(stream)
	}
	err = p.Wait()
	wg.Wait()
	if err != nil {
		return empty, err
	}
	return e.timbreOutputs(ctx, req, dir, outputs, rate, channels, input.DurationSeconds)
}

// Read properties before publication; the asset store then validates every WAV
// chunk and hashes the exact imported bytes again before making it visible.
func timbreWAV(f *os.File, n int64, rate uint32, channels uint16, duration float64) error {
	var h [12]byte
	if n < 44 || n > 1<<30 {
		return runtimeassets.ErrInvalid
	}
	if _, err := f.ReadAt(h[:], 0); err != nil || string(h[:4]) != "RIFF" || string(h[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(h[4:]))+8 != n {
		return runtimeassets.ErrInvalid
	}
	haveFmt, haveData := false, false
	var align uint16
	for pos := int64(12); pos < n; {
		var ch [8]byte
		if n-pos < 8 {
			return runtimeassets.ErrInvalid
		}
		if _, err := f.ReadAt(ch[:], pos); err != nil {
			return err
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		pos += 8
		if size > n-pos {
			return runtimeassets.ErrInvalid
		}
		switch string(ch[:4]) {
		case "fmt ":
			if haveFmt || haveData || size != 16 && size != 18 {
				return runtimeassets.ErrInvalid
			}
			var b [18]byte
			if _, err := f.ReadAt(b[:size], pos); err != nil {
				return err
			}
			bits := binary.LittleEndian.Uint16(b[14:])
			align = binary.LittleEndian.Uint16(b[12:])
			if binary.LittleEndian.Uint16(b[:]) != 1 || binary.LittleEndian.Uint16(b[2:]) != channels || binary.LittleEndian.Uint32(b[4:]) != rate || (bits != 8 && bits != 16 && bits != 24 && bits != 32) || align != channels*(bits/8) || binary.LittleEndian.Uint32(b[8:]) != rate*uint32(align) || size == 18 && binary.LittleEndian.Uint16(b[16:]) != 0 {
				return runtimeassets.ErrInvalid
			}
			haveFmt = true
		case "data":
			if !haveFmt || haveData || size == 0 || size%int64(align) != 0 || math.Abs(float64(size)/float64(rate*uint32(align))-duration) > 0.1 {
				return runtimeassets.ErrInvalid
			}
			haveData = true
		default:
			switch string(ch[:4]) {
			case "JUNK", "LIST", "bext":
			default:
				return runtimeassets.ErrInvalid
			}
		}
		pos += size
		if size%2 != 0 {
			var pad [1]byte
			if pos >= n {
				return runtimeassets.ErrInvalid
			}
			if _, err := f.ReadAt(pad[:], pos); err != nil || pad[0] != 0 {
				return runtimeassets.ErrInvalid
			}
			pos++
		}
	}
	if !haveFmt || !haveData {
		return runtimeassets.ErrInvalid
	}
	return nil
}

func (e *Executor) timbreOutputs(ctx context.Context, req runtimejobs.Request, dir string, names []string, rate uint32, channels uint16, duration float64) (runtimejobs.Result, error) {
	empty := runtimejobs.Result{}
	scope, err := safefs.Open(dir)
	if err != nil {
		return empty, err
	}
	defer scope.Close()
	staged := make([]*runtimeassets.ResultFile, 0, len(names))
	defer func() {
		for _, f := range staged {
			_ = f.Close()
		}
	}()
	for _, name := range names {
		if name != "vocals.wav" && name != "accompaniment.wav" && name != "converted.wav" {
			return empty, runtimeassets.ErrInvalid
		}
		leaf, err := scope.Derive(name)
		if err != nil {
			return empty, runtimeassets.ErrInvalid
		}
		src, entry, err := leaf.OpenFile(name)
		leaf.Close()
		if err != nil {
			return empty, runtimeassets.ErrInvalid
		}
		err = timbreWAV(src, entry.Size, rate, channels, duration)
		if err != nil {
			src.Close()
			return empty, err
		}
		dst, err := e.assets.CreateResult(ctx, req.TaskID, req.SessionID, runtimeassets.WAV, entry.Size)
		if err != nil {
			src.Close()
			return empty, err
		}
		staged = append(staged, dst)
		n, copyErr := io.Copy(dst, io.LimitReader(timbreContextReader{ctx, src}, entry.Size+1))
		err = errors.Join(copyErr, src.Close())
		if err != nil {
			return empty, err
		}
		if n != entry.Size {
			return empty, runtimeassets.ErrInvalid
		}
	}
	result := runtimejobs.Result{Component: req.Body.Component}
	for _, dst := range staged {
		a, err := e.assets.PublishResult(ctx, req.TaskID, dst)
		if err != nil {
			return empty, err
		}
		m := a.Metadata()
		if m.SampleRate != rate || m.Channels != channels || math.Abs(m.DurationSeconds-duration) > 0.1 {
			return empty, runtimeassets.ErrInvalid
		}
		result.Media = append(result.Media, runtimejobs.MediaResult{ID: a.ID(), Bytes: m.Bytes, SHA256: m.SHA256, SampleRate: int(m.SampleRate), Channels: int(m.Channels), DurationMS: int64(math.Round(m.DurationSeconds * 1000))})
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return result, nil
}
