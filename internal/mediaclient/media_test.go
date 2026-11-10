package mediaclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

func TestFaceRequiresExpectedImageSHA(t *testing.T) {
	for _, mode := range []string{"replaced", "wrong", "missing", "short", "nonhex", "success", "uppercase"} {
		t.Run(mode, func(t *testing.T) {
			c, f, receipts := fixture(t, "image.face")
			var b bytes.Buffer
			if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			raw := append([]byte(nil), b.Bytes()...)
			path := input(t, raw)
			expected := digest(raw)
			switch mode {
			case "replaced":
				b.Reset()
				if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong":
				expected = strings.Repeat("0", 64)
			case "missing":
				expected = ""
			case "short":
				expected = expected[:63]
			case "nonhex":
				expected = strings.Repeat("z", 64)
			case "uppercase":
				expected = strings.ToUpper(expected)
			}
			if mode == "success" || mode == "uppercase" {
				f.onUpload = func() {
					if err := os.WriteFile(path, []byte("replaced after verification"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.result.Face = integration.RuntimeFrontal
			got, err := c.Face(context.Background(), path, expected)
			if mode == "success" || mode == "uppercase" {
				if err != nil || got != integration.RuntimeFrontal || f.uploads != 1 || f.submits != 1 || len(*receipts) != 5 || !bytes.Equal(f.uploaded, raw) {
					t.Fatalf("incomplete success: %q %v uploads=%d submits=%d receipts=%d", got, err, f.uploads, f.submits, len(*receipts))
				}
				return
			}
			if !errors.Is(err, errMedia) || got != "" || f.uploads != 0 || f.submits != 0 || len(*receipts) != 0 {
				t.Fatalf("invalid SHA started work: %q %v uploads=%d submits=%d receipts=%d", got, err, f.uploads, f.submits, len(*receipts))
			}
		})
	}
}

func TestPCMInputFormats(t *testing.T) {
	for _, bits := range []int{8, 16, 24, 32} {
		raw := wav(16000, 2, 16000)
		raw = append(raw[:44:44], make([]byte, 16000*2*bits/8)...)
		binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
		binary.LittleEndian.PutUint32(raw[28:], uint32(16000*2*bits/8))
		binary.LittleEndian.PutUint16(raw[32:], uint16(2*bits/8))
		binary.LittleEndian.PutUint16(raw[34:], uint16(bits))
		binary.LittleEndian.PutUint32(raw[40:], uint32(len(raw)-44))
		p, e := readWAV(bytes.NewReader(raw), int64(len(raw)))
		if e != nil || p.duration() != 1 {
			t.Fatalf("PCM%d: %v", bits, e)
		}
	}
	for _, field := range []int{20, 28, 32, 34} {
		raw := wav(16000, 1, 16000)
		raw[field] = 255
		if _, e := readWAV(bytes.NewReader(raw), int64(len(raw))); e == nil {
			t.Fatalf("accepted invalid format field %d", field)
		}
	}
}
func TestDurationStrictBoundary(t *testing.T) {
	for _, frames := range []int64{39690, 48510} {
		if durationWithin(wavProperties{44100, 2, frames}, wavProperties{48000, 1, 48000}) {
			t.Fatal("accepted exactly 100ms")
		}
	}
	for _, frames := range []int64{39691, 48509} {
		if !durationWithin(wavProperties{44100, 2, frames}, wavProperties{48000, 1, 48000}) {
			t.Fatal("rejected less than 100ms")
		}
	}
}
func TestReceiptPersistenceAndMutationIsolation(t *testing.T) {
	c, f, src, v, a := setupSeparate(t)
	var stages []Receipt
	c.record = func(r Receipt) error {
		stages = append(stages, r)
		if r.Body.Separate != nil {
			r.Body.Separate.AssetID = "mutated"
		}
		return nil
	}
	if _, e := c.Separate(context.Background(), src, v, a); e != nil {
		t.Fatal(e)
	}
	if len(stages) != 5 || stages[0].Snapshot != nil || stages[1].Snapshot == nil || stages[2].Snapshot.State != "succeeded" || f.body.Separate.AssetID == "mutated" {
		t.Fatal("receipt ownership/stages")
	}
	c, f, src, v, a = setupSeparate(t)
	expected := errors.New("receipt store unavailable")
	c.record = func(Receipt) error { return expected }
	if _, e := c.Separate(context.Background(), src, v, a); !errors.Is(e, expected) || f.submits != 0 {
		t.Fatal("submitted before durable receipt", e)
	}
}
func TestBoundInputAndOutputAliases(t *testing.T) {
	c, f, src, _, a := setupSeparate(t)
	before, e := os.ReadFile(src)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Separate(context.Background(), src, src, a); e == nil {
		t.Fatal("accepted source destination alias")
	}
	after, _ := os.ReadFile(src)
	if !bytes.Equal(before, after) || f.submits != 0 {
		t.Fatal("modified input")
	}
	out := filepath.Join(t.TempDir(), "same.wav")
	if _, e = c.Separate(context.Background(), src, out, out); e == nil {
		t.Fatal("accepted duplicate output path")
	}
	if _, e = os.Stat(out); !os.IsNotExist(e) {
		t.Fatal("left duplicate reservation")
	}
}
func TestImageByteBoundAndIdentity(t *testing.T) {
	for _, kind := range []string{"oversize", "digest", "length", "dimensions"} {
		t.Run(kind, func(t *testing.T) {
			c, f, _ := fixture(t, "image.face")
			var b bytes.Buffer
			_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			raw := b.Bytes()
			switch kind {
			case "oversize":
				raw = make([]byte, (8<<20)+1)
			case "digest":
				f.mutateAsset = func(a *integration.RuntimeAsset) { a.Metadata.SHA256 = strings.Repeat("0", 64) }
			case "length":
				f.mutateAsset = func(a *integration.RuntimeAsset) { a.Metadata.Bytes = "1" }
			case "dimensions":
				f.mutateAsset = func(a *integration.RuntimeAsset) { a.Metadata.Width++ }
			}
			if _, e := c.Face(context.Background(), input(t, raw), digest(raw)); e == nil || f.submits != 0 {
				t.Fatal("accepted invalid image metadata")
			}
		})
	}
}
func TestFaceRejectsMixedOutput(t *testing.T) {
	c, f, _ := fixture(t, "image.face")
	f.result.Face = integration.RuntimeFrontal
	f.result.Events = []*integration.RuntimeRawEvent{}
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	if got, e := c.Face(context.Background(), input(t, b.Bytes()), digest(b.Bytes())); e == nil || got != "" {
		t.Fatal("accepted mixed result shape")
	}
}

func TestInvalidFrozenBindings(t *testing.T) {
	c, f, _ := fixture(t, "image.face")
	valid := c.bindings["image.face"]
	for _, kind := range []string{"component", "revision", "digest", "duplicate", "grant"} {
		t.Run(kind, func(t *testing.T) {
			b := valid
			g := f.g
			bindings := []Binding{b}
			switch kind {
			case "component":
				bindings[0].Component.ID = "demucs-runner"
			case "revision":
				bindings[0].Binding.Revision++
			case "digest":
				bindings[0].Component.ManifestSHA256 = "invalid"
			case "duplicate":
				bindings = append(bindings, b)
			case "grant":
				g.Permission = integration.PermissionReadOnly
			}
			if _, e := newClient(f, func() integration.SessionGrant { return g }, bindings, func(Receipt) error { return nil }, safefs.PersistParent); e == nil {
				t.Fatal("accepted invalid frozen binding")
			}
		})
	}
}
