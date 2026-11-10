package runtimeadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
)

func faceTestRequest() runtimejobs.Request {
	return runtimejobs.Request{TaskID: "task", SessionID: "session", Binding: integration.ExecutionBindingRef{Scene: "image.face"}, Body: runtimejobs.Body{Component: integration.ExecutionComponentRef{ID: "haar-consumer", Version: "v1", Protocol: "haar/v1"}, Face: &runtimejobs.FaceInput{AssetID: "not-opened"}}}
}
func faceTestResolved() Resolved {
	return Resolved{Manifest: components.Manifest{ComponentID: "haar-consumer", Version: "v1", Packaging: "go", Protocols: []string{"haar/v1"}}}
}
func TestFaceRequiresHaarProtocolBeforeModelLoad(t *testing.T) {
	for _, tc := range []struct {
		request  string
		manifest []string
	}{
		{"", []string{"haar/v1"}}, {"other/v1", []string{"haar/v1"}}, {"haar/v1", nil}, {"haar/v1", []string{"other/v1"}},
	} {
		req, resolved := faceTestRequest(), faceTestResolved()
		req.Body.Component.Protocol, resolved.Manifest.Protocols = tc.request, tc.manifest
		got, err := timbreTestExecutor(t).face(context.Background(), req, resolved, nil)
		if err == nil || err.Error() != "center_component_missing" || got.Face != "" {
			t.Fatalf("wrong protocol reached model load: %+v %v", got, err)
		}
	}
}

func TestFaceRejectBindingAndCancellation(t *testing.T) {
	e := timbreTestExecutor(t)
	req := faceTestRequest()
	r := faceTestResolved()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := e.face(ctx, req, r, nil); !errors.Is(err, context.Canceled) || got.Face != "" {
		t.Fatal(got, err)
	}
	root := t.TempDir()
	for _, models := range [][]ModelFile{nil, {{"haar-frontal", filepath.Join(root, "front.xml")}}, {{"haar-frontal", filepath.Join(root, "front.xml")}, {"haar-frontal", filepath.Join(root, "profile.xml")}}, {{"haar-frontal", filepath.Join(root, "same.xml")}, {"haar-profile", filepath.Join(root, "same.xml")}}, {{"haar-frontal", "relative.xml"}, {"haar-profile", filepath.Join(root, "profile.xml")}}} {
		r.Models = models
		got, err := e.face(context.Background(), req, r, nil)
		if err == nil || got.Face != "" {
			t.Fatal("invalid binding accepted", got, err)
		}
	}
}
func TestFaceBoundXMLLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bound.xml")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate((8 << 20) + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = faceBoundXML(context.Background(), path); err == nil {
		t.Fatal("oversized XML read")
	}
	if err = os.WriteFile(path, []byte("invalid XML"), 0600); err != nil {
		t.Fatal(err)
	}
	r := faceTestResolved()
	r.Models = []ModelFile{{"haar-frontal", path}, {"haar-profile", filepath.Join(dir, "missing.xml")}}
	if got, err := timbreTestExecutor(t).face(context.Background(), faceTestRequest(), r, nil); err == nil || got.Face != "" {
		t.Fatal(got, err)
	}
}

// Real model inference is opt-in. A profile sample requires a separate explicit
// input with externally reviewed provenance; a mirrored derivative is not a second real sample.
func TestFaceRealSharedHaar(t *testing.T) {
	if os.Getenv("FENGSHEN_TEST_REAL_HAAR") != "1" {
		t.Skip("set FENGSHEN_TEST_REAL_HAAR=1 for actual bound XML/JPEG inference")
	}
	root, err := filepath.Abs("../../assets/haar")
	if err != nil {
		t.Fatal(err)
	}
	r := faceTestResolved()
	r.Models = []ModelFile{{"haar-frontal", filepath.Join(root, "models", "haarcascade_frontalface_default.xml")}, {"haar-profile", filepath.Join(root, "models", "haarcascade_profileface.xml")}}
	picture, err := os.ReadFile(filepath.Join(root, "testdata", "messi5.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	var blank bytes.Buffer
	if err = png.Encode(&blank, image.NewGray(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, data []byte, want runtimejobs.FaceResult) {
		t.Helper()
		e := timbreTestExecutor(t)
		req := faceTestRequest()
		ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		a, err := e.assets.Upload(ctx, req.TaskID, req.SessionID, runtimeassets.Image, int64(len(data)), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Body.Face.AssetID = a.ID()
		got, err := e.face(ctx, req, r, nil)
		if err != nil || got.Face != want {
			t.Fatalf("actual Haar got=%s want=%s err=%v input_sha256=%x", got.Face, want, err, sha256.Sum256(data))
		}
		t.Logf("actual Go Haar face=%s input_sha256=%x", got.Face, sha256.Sum256(data))
		req.TaskID = "other"
		if got, err = e.face(ctx, req, r, nil); !errors.Is(err, runtimeassets.ErrForbidden) || got.Face != "" {
			t.Fatal("cross task", got, err)
		}
		req.TaskID = "task"
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		got, err = e.face(cancelled, req, r, func(runtimejobs.Progress) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) || got.Face != "" {
			t.Fatal("cancel after load", got, err)
		}
	}
	t.Run("real_messi5_frontal", func(t *testing.T) { run(t, picture, runtimejobs.Frontal) })
	t.Run("generated_blank_no_face", func(t *testing.T) { run(t, blank.Bytes(), runtimejobs.NoFace) })
	t.Run("real_profile", func(t *testing.T) {
		path := os.Getenv("FENGSHEN_TEST_REAL_HAAR_PROFILE_JPEG")
		if path == "" {
			t.Skip("profile NOT RUN: set FENGSHEN_TEST_REAL_HAAR_PROFILE_JPEG to an absolute genuine profile JPEG; review source/timestamp/SHA, no synthetic images")
		}
		data, err := faceProfileJPEG(path)
		if err != nil {
			t.Fatal(err)
		}
		mirrored, err := faceProfileMirror(data)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("profile source=%s sha256=%x; mirror is a lossless PNG derivative of this one sample", path, sha256.Sum256(data))
		t.Run("original", func(t *testing.T) { run(t, data, runtimejobs.Profile) })
		t.Run("horizontal_reflection", func(t *testing.T) { run(t, mirrored, runtimejobs.Profile) })
	})
}

func faceProfileJPEG(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("profile JPEG must be an explicit absolute path")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 8<<20 {
		return nil, fmt.Errorf("profile JPEG must be a nonempty regular file of at most 8 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != st.Size() {
		return nil, fmt.Errorf("profile JPEG changed or exceeds the byte limit")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
		return nil, fmt.Errorf("profile must be a valid JPEG with bounded dimensions")
	}
	return data, nil
}

func faceProfileMirror(data []byte) ([]byte, error) {
	source, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := source.Bounds()
	flipped := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			flipped.Set(x, y, source.At(b.Max.X-1-x, b.Min.Y+y))
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, flipped); err != nil {
		return nil, err
	}
	if encoded.Len() > 8<<20 {
		return nil, fmt.Errorf("reflected profile exceeds the image byte limit")
	}
	return encoded.Bytes(), nil
}

func TestFaceRealSharedHaarDefaultDoesNotResolveProfile(t *testing.T) {
	t.Setenv("FENGSHEN_TEST_REAL_HAAR", "0")
	t.Setenv("FENGSHEN_TEST_REAL_HAAR_PROFILE_JPEG", "invalid-relative-input.jpg")
	if !t.Run("disabled_before_input_or_model_load", TestFaceRealSharedHaar) {
		t.Fatal("disabled real Haar test touched profile input or models")
	}
}

// Generated pixels test only the reflection helper, never real Haar coverage.
func TestFaceProfileMirrorPreservesHorizontalPixels(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			src.Set(x, y, color.NRGBA{R: uint8(40 * x), G: uint8(70 * y), B: 30, A: 255})
		}
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, src, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "helper-only.jpg")
	if err := os.WriteFile(path, jpegData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := faceProfileJPEG(path)
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := faceProfileMirror(data)
	if err != nil {
		t.Fatal(err)
	}
	original, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := png.Decode(bytes.NewReader(mirrored))
	if err != nil {
		t.Fatal(err)
	}
	if flipped.Bounds() != original.Bounds() {
		t.Fatal("reflection dimensions changed")
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			want := color.NRGBAModel.Convert(original.At(4-x, y))
			got := color.NRGBAModel.Convert(flipped.At(x, y))
			if got != want {
				t.Fatal("reflection changed decoded pixels")
			}
		}
	}
}

func TestFaceProfileJPEGRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"relative.jpg", filepath.Join(root, "missing.jpg"), root} {
		if _, err := faceProfileJPEG(path); err == nil {
			t.Fatalf("invalid profile path accepted: %s", path)
		}
	}
	path := filepath.Join(root, "invalid.jpg")
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("not a JPEG"), pngData.Bytes()} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := faceProfileJPEG(path); err == nil {
			t.Fatal("invalid JPEG accepted")
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate((8 << 20) + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := faceProfileJPEG(path); err == nil {
		t.Fatal("oversized profile accepted")
	}
}
