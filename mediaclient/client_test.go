package mediaclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

type fakeTransport struct {
	g                                integration.SessionGrant
	b                                Binding
	body                             integration.RuntimeBody
	submits, uploads, cancels        int
	result                           integration.RuntimeResult
	outputs                          map[string][]byte
	submitErr, eventErr, downloadErr error
	onEvents                         func(context.Context) error
	mutateAsset                      func(*integration.RuntimeAsset)
	mutateSnapshot                   func(*integration.RuntimeSnapshot)
	cancelDeadline                   bool
	onUpload                         func()
	acks                             int
	onAck                            func() error
	uploaded                         []byte
}

func digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func (f *fakeTransport) Upload(ctx context.Context, typ string, n int64, r io.Reader) (integration.RuntimeAsset, error) {
	f.uploads++
	if f.onUpload != nil {
		f.onUpload()
	}
	raw, e := io.ReadAll(r)
	f.uploaded = append([]byte(nil), raw...)
	if e != nil {
		return integration.RuntimeAsset{}, e
	}
	a := integration.RuntimeAsset{ID: strings.Repeat(strconv.Itoa(f.uploads), 64), Metadata: integration.RuntimeAssetMetadata{Bytes: strconv.FormatInt(n, 10), SHA256: digest(raw)}}
	if typ == "audio/wav" {
		p, e := readWAV(bytes.NewReader(raw), int64(len(raw)))
		if e != nil {
			return a, e
		}
		a.Metadata.Kind = "wav"
		a.Metadata.SampleRate = uint32(p.rate)
		a.Metadata.Channels = uint16(p.channels)
		a.Metadata.DurationSeconds = p.duration()
	} else {
		cfg, _, e := image.DecodeConfig(bytes.NewReader(raw))
		if e != nil {
			return a, e
		}
		a.Metadata.Kind = "image"
		a.Metadata.Width = cfg.Width
		a.Metadata.Height = cfg.Height
	}
	if f.mutateAsset != nil {
		f.mutateAsset(&a)
	}
	return a, nil
}
func (f *fakeTransport) Submit(ctx context.Context, id string, b Binding, body integration.RuntimeBody) (*job, error) {
	f.submits++
	f.b = b
	f.body = body
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	s := integration.RuntimeSnapshot{ID: strings.Repeat("a", 64), SessionID: f.g.SessionID, TaskID: f.g.TaskID, RequestID: id, Binding: b.Binding, BodySHA256: bodyHash(b, body), State: "queued", CreatedAt: time.Now(), Cursor: 1}
	if f.mutateSnapshot != nil {
		f.mutateSnapshot(&s)
	}
	return &job{snapshot: s}, nil
}
func (f *fakeTransport) Events(ctx context.Context, j *job, c uint64, emit func(integration.RuntimeEvent) error) error {
	if f.onEvents != nil {
		return f.onEvents(ctx)
	}
	return f.eventErr
}
func (f *fakeTransport) Query(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	s := j.snapshot
	s.State = "succeeded"
	s.FinishedAt = s.CreatedAt.Add(time.Second)
	s.Cursor++
	return s, nil
}
func (f *fakeTransport) Result(context.Context, *job) (integration.RuntimeResult, error) {
	r := f.result
	if r.Component.ID == "" {
		r.Component = f.b.Component
	}
	return r, nil
}
func (f *fakeTransport) Cancel(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	f.cancels++
	d, ok := ctx.Deadline()
	f.cancelDeadline = ok && time.Until(d) <= 3*time.Second && ctx.Err() == nil
	return f.Query(ctx, j)
}
func (f *fakeTransport) Acknowledge(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	f.acks++
	if f.onAck != nil {
		if e := f.onAck(); e != nil {
			return integration.RuntimeSnapshot{}, e
		}
	}
	s, e := f.Query(ctx, j)
	s.Acknowledged = true
	return s, e
}
func (f *fakeTransport) DownloadWAV(ctx context.Context, j *job, id string, w io.Writer) error {
	_, e := w.Write(f.outputs[id])
	return errors.Join(e, f.downloadErr)
}
func wav(rate, channels, frames int) []byte {
	b := make([]byte, 44+frames*channels*2)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(b)-44))
	return b
}
func fixture(t *testing.T, scene string) (*Client, *fakeTransport, *[]Receipt) {
	t.Helper()
	component := "demucs-runner"
	if scene == "timbre.convert" {
		component = "seedvc-runner"
	}
	if scene == "image.face" {
		component = "haar-consumer"
	}
	b := Binding{integration.ExecutionBindingRef{Scene: scene, Revision: 1, SHA256: strings.Repeat("b", 64)}, integration.ExecutionComponentRef{ID: component, Version: "v1", ManifestSHA256: strings.Repeat("c", 64), Protocol: "test/v1"}}
	g := integration.SessionGrant{SessionID: "session", TaskID: "task", InstanceID: "instance", Permission: integration.PermissionRuntime, Capabilities: []string{scene}, Bindings: []integration.ExecutionBindingRef{b.Binding}}
	f := &fakeTransport{g: g, outputs: map[string][]byte{}}
	receipts := []Receipt{}
	receiptPath := filepath.Join(t.TempDir(), "receipt.json")
	c, e := newClient(f, func() integration.SessionGrant { return f.g }, []Binding{b}, func(r Receipt) error {
		receipts = append(receipts, r)
		raw, e := json.Marshal(r)
		if e != nil {
			return e
		}
		return os.WriteFile(receiptPath, raw, 0600)
	}, safefs.PersistParent)
	if e != nil {
		t.Fatal(e)
	}
	return c, f, &receipts
}
func input(t *testing.T, raw []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input")
	if e := os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func media(raw []byte, id string, rate, channels int, ms int64) integration.RuntimeMediaResult {
	return integration.RuntimeMediaResult{ID: id, Bytes: strconv.Itoa(len(raw)), SHA256: digest(raw), SampleRate: rate, Channels: channels, DurationMS: ms}
}
func setupSeparate(t *testing.T) (*Client, *fakeTransport, string, string, string) {
	c, f, _ := fixture(t, "timbre.separate")
	a := wav(44100, 2, 44100)
	b := append([]byte(nil), a...)
	b[len(b)-1] = 1
	f.outputs["opaque-z"] = a
	f.outputs["opaque-a"] = b
	f.result.Media = []integration.RuntimeMediaResult{media(a, "opaque-z", 44100, 2, 1000), media(b, "opaque-a", 44100, 2, 1000)}
	dir := t.TempDir()
	return c, f, input(t, wav(48000, 1, 48000)), filepath.Join(dir, "vocals.wav"), filepath.Join(dir, "accompaniment.wav")
}
func TestAudioDurableBeforeAckAndUnknownAckKeepsOutputs(t *testing.T) {
	for _, mode := range []string{"success", "durable-failed", "sync-failed", "directory-failed", "download-failed", "unknown-ack"} {
		t.Run(mode, func(t *testing.T) {
			c, f, src, v, a := setupSeparate(t)
			synced, durable := 0, false
			sentinel := errors.New("offline injected failure")
			c.syncOutput = func(file *os.File) error {
				if mode == "sync-failed" {
					return sentinel
				}
				synced++
				return file.Sync()
			}
			published := 0
			c.persistOutput = func(path string) error {
				if synced != 2 {
					t.Fatal("directory publication before WAV syncs")
				}
				if mode == "directory-failed" {
					return sentinel
				}
				if e := safefs.PersistParent(path); e != nil {
					return e
				}
				published++
				return nil
			}
			if mode == "download-failed" {
				f.downloadErr = sentinel
			}
			path := filepath.Join(t.TempDir(), "completion.json")
			c.record = func(r Receipt) error {
				if r.Completion == nil {
					return nil
				}
				if synced != 2 || published != 2 {
					t.Fatal("receipt before WAV syncs and directory publication")
				}
				for _, p := range []string{v, a} {
					file, e := os.OpenFile(p, os.O_RDWR, 0)
					if e != nil {
						t.Fatal("output not closed", e)
					}
					file.Close()
				}
				if mode == "durable-failed" {
					return sentinel
				}
				raw, e := json.Marshal(r)
				if e != nil {
					return e
				}
				if e = os.WriteFile(path, raw, 0600); e != nil {
					return e
				}
				durable = true
				return nil
			}
			f.onAck = func() error {
				if !durable {
					t.Fatal("Ack before durable complete result")
				}
				if mode == "unknown-ack" {
					return sentinel
				}
				return nil
			}
			_, e := c.Separate(context.Background(), src, v, a)
			if (e == nil) != (mode == "success") {
				t.Fatal(mode, e)
			}
			wantAck := mode == "success" || mode == "unknown-ack"
			if (f.acks == 1) != wantAck || f.submits != 1 {
				t.Fatal("Ack/replay", f.acks, f.submits)
			}
			keep := mode == "success" || mode == "unknown-ack" || mode == "durable-failed"
			for _, p := range []string{v, a} {
				_, e := os.Stat(p)
				if (e == nil) != keep {
					t.Fatal("wrong output cleanup", p, e)
				}
			}
		})
	}
}

func TestSeparateOrderedOpaqueResults(t *testing.T) {
	c, f, src, v, a := setupSeparate(t)
	device, e := c.Separate(context.Background(), src, v, a)
	if e != nil || device != "" {
		t.Fatalf("device=%q err=%v", device, e)
	}
	for p, id := range map[string]string{v: "opaque-z", a: "opaque-a"} {
		raw, e := os.ReadFile(p)
		if e != nil || !bytes.Equal(raw, f.outputs[id]) {
			t.Fatal("incorrect track order", e)
		}
	}
	if f.submits != 1 || f.cancels != 0 {
		t.Fatal("unexpected replay or cancellation")
	}
}
func TestConvertZeroPitch(t *testing.T) {
	c, f, _ := fixture(t, "timbre.convert")
	raw := wav(22050, 1, 22050)
	f.outputs["opaque"] = raw
	f.result.Media = []integration.RuntimeMediaResult{media(raw, "opaque", 22050, 1, 1000)}
	src := input(t, wav(48000, 2, 48000))
	ref := input(t, wav(16000, 1, 8000))
	out := filepath.Join(t.TempDir(), "out.wav")
	if e := c.Convert(context.Background(), src, ref, out); e != nil {
		t.Fatal(e)
	}
	if f.body.Convert == nil || f.body.Convert.Pitch != 0 || f.body.Convert.AssetID == f.body.Convert.ReferenceAssetID {
		t.Fatal("invalid conversion body")
	}
}
func TestInvalidOutputsCleanOwnedFiles(t *testing.T) {
	for _, kind := range []string{"shape", "component", "duplicate", "length", "digest", "corrupt", "rate", "channels", "duration", "boundary", "partial"} {
		t.Run(kind, func(t *testing.T) {
			c, f, src, v, a := setupSeparate(t)
			switch kind {
			case "shape":
				f.result.Face = integration.RuntimeNoFace
			case "component":
				f.result.Component = integration.ExecutionComponentRef{ID: "wrong"}
			case "duplicate":
				f.result.Media[1].ID = f.result.Media[0].ID
			case "length":
				f.result.Media[0].Bytes = "1"
			case "digest":
				f.result.Media[0].SHA256 = strings.Repeat("0", 64)
			case "corrupt":
				f.outputs["opaque-z"][0] = 'X'
			case "rate":
				f.result.Media[0].SampleRate = 48000
			case "channels":
				f.result.Media[0].Channels = 1
			case "duration":
				f.result.Media[0].DurationMS = 1100
			case "boundary":
				raw := wav(44100, 2, 48510)
				f.outputs["opaque-z"] = raw
				f.result.Media[0] = media(raw, "opaque-z", 44100, 2, 1100)
			case "partial":
				f.downloadErr = io.ErrUnexpectedEOF
			}
			if _, e := c.Separate(context.Background(), src, v, a); e == nil {
				t.Fatal("accepted invalid output")
			}
			for _, p := range []string{v, a} {
				if _, e := os.Stat(p); !os.IsNotExist(e) {
					t.Fatal("left partial", p)
				}
			}
		})
	}
}
func TestExistingOutputUntouched(t *testing.T) {
	c, f, src, v, a := setupSeparate(t)
	if e := os.WriteFile(a, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Separate(context.Background(), src, v, a); e == nil {
		t.Fatal("overwrote existing output")
	}
	raw, _ := os.ReadFile(a)
	if string(raw) != "keep" || f.submits != 0 || f.uploads != 0 {
		t.Fatal("existing output changed or remote work started")
	}
	if _, e := os.Stat(v); !os.IsNotExist(e) {
		t.Fatal("left reserved output")
	}
}
func TestUncertainSubmitAndSSE(t *testing.T) {
	for _, post := range []bool{true, false} {
		c, f, r := fixture(t, "image.face")
		var b bytes.Buffer
		_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
		if post {
			f.submitErr = errors.New("center_transport_failed")
		} else {
			f.eventErr = errors.New("center_transport_failed")
		}
		if _, e := c.Face(context.Background(), input(t, b.Bytes()), digest(b.Bytes())); e == nil {
			t.Fatal("connection loss succeeded")
		}
		if f.submits != 1 || f.cancels != 0 || len(*r) == 0 || (*r)[0].RequestID == "" {
			t.Fatal("lost receipt or retried uncertain write")
		}
	}
}
func TestCancelOnlyAdmittedJobBounded(t *testing.T) {
	c, f, _ := fixture(t, "image.face")
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	ctx, cancel := context.WithCancel(context.Background())
	f.onEvents = func(context.Context) error { cancel(); return context.Canceled }
	_, e := c.Face(ctx, input(t, b.Bytes()), digest(b.Bytes()))
	if !errors.Is(e, context.Canceled) || f.cancels != 1 || !f.cancelDeadline {
		t.Fatal("unbounded or missing job cancellation", e)
	}
}
func TestScopeChangesAndCrossTaskReceipts(t *testing.T) {
	for _, kind := range []string{"task", "session", "binding", "receipt-task", "receipt-binding"} {
		t.Run(kind, func(t *testing.T) {
			c, f, _ := fixture(t, "image.face")
			switch kind {
			case "task":
				f.g.TaskID = "other"
			case "session":
				f.g.SessionID = "other"
			case "binding":
				f.g.Bindings[0].Revision++
			case "receipt-task":
				f.mutateSnapshot = func(s *integration.RuntimeSnapshot) { s.TaskID = "other" }
			case "receipt-binding":
				f.mutateSnapshot = func(s *integration.RuntimeSnapshot) { s.Binding.Revision++ }
			}
			var b bytes.Buffer
			_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			if _, e := c.Face(context.Background(), input(t, b.Bytes()), digest(b.Bytes())); e == nil {
				t.Fatal("accepted scope change")
			}
		})
	}
}
func TestFaceTypedAndInvalid(t *testing.T) {
	for _, kind := range []integration.RuntimeFaceResult{integration.RuntimeFrontal, integration.RuntimeProfile, integration.RuntimeNoFace, "invalid", ""} {
		c, f, _ := fixture(t, "image.face")
		f.result.Face = kind
		var b bytes.Buffer
		_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
		got, e := c.Face(context.Background(), input(t, b.Bytes()), digest(b.Bytes()))
		valid := kind == integration.RuntimeFrontal || kind == integration.RuntimeProfile || kind == integration.RuntimeNoFace
		if valid && (e != nil || got != kind) || !valid && e == nil {
			t.Fatalf("face %q: %q %v", kind, got, e)
		}
	}
}
func TestInvalidInputAndUploadIdentity(t *testing.T) {
	for _, kind := range []string{"corrupt", "truncated", "channels", "hash", "bytes", "directory", "image-large", "image-corrupt"} {
		t.Run(kind, func(t *testing.T) {
			c, f, src, v, a := setupSeparate(t)
			switch kind {
			case "corrupt":
				src = input(t, []byte("not WAV"))
			case "truncated":
				src = input(t, wav(16000, 1, 16000)[:44])
			case "channels":
				src = input(t, wav(16000, 3, 16000))
			case "hash":
				f.mutateAsset = func(a *integration.RuntimeAsset) { a.Metadata.SHA256 = strings.Repeat("0", 64) }
			case "bytes":
				f.mutateAsset = func(a *integration.RuntimeAsset) { a.Metadata.Bytes = "1" }
			case "directory":
				src = t.TempDir()
			case "image-large", "image-corrupt":
				c, f, _ = fixture(t, "image.face")
				raw := []byte("invalid")
				if kind == "image-large" {
					var b bytes.Buffer
					_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, 4097, 1)))
					raw = b.Bytes()
				}
				if _, e := c.Face(context.Background(), input(t, raw), digest(raw)); e == nil {
					t.Fatal("accepted bad image")
				}
				return
			}
			if _, e := c.Separate(context.Background(), src, v, a); e == nil {
				t.Fatal("accepted invalid media")
			}
			if f.submits != 0 {
				t.Fatal("submitted invalid media")
			}
		})
	}
}
func TestConstructorRejectsOtherScenes(t *testing.T) {
	c, f, _ := fixture(t, "image.face")
	b := c.bindings["image.face"]
	b.Binding.Scene = "highlight.score"
	b.Component.ID = "llama-runtime"
	f.g.Capabilities = []string{b.Binding.Scene}
	f.g.Bindings = []integration.ExecutionBindingRef{b.Binding}
	if _, e := newClient(f, func() integration.SessionGrant { return f.g }, []Binding{b}, func(Receipt) error { return nil }, safefs.PersistParent); e == nil {
		t.Fatal("accepted unrelated scene")
	}
}
