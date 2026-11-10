package runtimeassets

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type interrupted struct{}

func (interrupted) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type gateReader struct {
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
	r       *bytes.Reader
}

func (g *gateReader) Read(b []byte) (int, error) {
	g.once.Do(func() { close(g.entered); <-g.resume })
	return g.r.Read(b)
}
func TestConcurrentReservationAndRollback(t *testing.T) {
	s, e := newWithLimits(t.TempDir(), limits{64, 64, 64, 64, 2})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	g := &gateReader{entered: make(chan struct{}), resume: make(chan struct{}), r: bytes.NewReader(wav())}
	done := make(chan error, 1)
	go func() { _, e := s.Upload(ctx, "t", "s", WAV, 48, g); done <- e }()
	<-g.entered
	if _, e = s.Upload(ctx, "t", "s2", WAV, 48, bytes.NewReader(wav())); !errors.Is(e, ErrFull) {
		t.Fatalf("reservation not held: %v", e)
	}
	close(g.resume)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = s.ReleaseSession("s"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upload(ctx, "t", "s2", WAV, 48, interrupted{}); e == nil {
		t.Fatal("interrupted")
	}
	if _, e = s.Upload(ctx, "t", "s2", WAV, 48, bytes.NewReader(wav())); e != nil {
		t.Fatal(e)
	}
}
func TestImagesAndWAVFormats(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	var good bytes.Buffer
	if e = png.Encode(&good, image.NewRGBA(image.Rect(0, 0, 2, 2))); e != nil {
		t.Fatal(e)
	}
	a, e := s.Upload(ctx, "t", "s", Image, int64(good.Len()), bytes.NewReader(good.Bytes()))
	if e != nil || a.Metadata().Width != 2 {
		t.Fatalf("image: %v", e)
	}
	broken := good.Bytes()[:good.Len()/2]
	if _, e = s.Upload(ctx, "t", "s", Image, int64(len(broken)), bytes.NewReader(broken)); e == nil {
		t.Fatal("truncated image")
	}
	var big bytes.Buffer
	if e = png.Encode(&big, image.NewGray(image.Rect(0, 0, 4097, 1))); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upload(ctx, "t", "s", Image, int64(big.Len()), bytes.NewReader(big.Bytes())); e == nil {
		t.Fatal("dimension")
	}
	for _, offset := range []int{4, 20, 22, 24, 28, 32, 34, 40} {
		b := wav()
		b[offset] = 255
		if _, e = s.Upload(ctx, "t", "s", WAV, 48, bytes.NewReader(b)); e == nil {
			t.Fatalf("invalid WAV at %d", offset)
		}
	}
	for _, k := range []Kind{WAV, Image, JSON, "exe"} {
		if _, e = s.Upload(ctx, "t", "s", k, s.max(k)+1, bytes.NewReader(nil)); e == nil {
			t.Fatal("size/kind")
		}
	}
}
func TestResultOwnershipJSONAndCorruption(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	other, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	ctx := context.Background()
	r, e := s.CreateResult(ctx, "t", "s", JSON, 2)
	if e != nil {
		t.Fatal(e)
	}
	r.Write([]byte("{}"))
	if _, e = other.PublishResult(ctx, "t", r); e == nil {
		t.Fatal("foreign store")
	}
	if _, e = s.PublishResult(ctx, "wrong", r); e == nil {
		t.Fatal("foreign task")
	}
	a, e := s.PublishResult(ctx, "t", r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.PublishResult(ctx, "t", r); e == nil {
		t.Fatal("republish")
	}
	// A component or other same-user writer is not a trusted source of metadata.
	f, e := s.dir.OpenFile(a.ID(), os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte("[]"))
	f.Close()
	if _, _, e = s.Download(ctx, "t", a.ID()); e == nil {
		t.Fatal("changed result")
	}
	for _, b := range []string{"{]", "{}{}", "\"../../weights\" trailing"} {
		r, e = s.CreateResult(ctx, "t", "s", JSON, int64(len(b)))
		if e != nil {
			t.Fatal(e)
		}
		r.Write([]byte(b))
		if _, e = s.PublishResult(ctx, "t", r); e == nil {
			t.Fatal("invalid json")
		}
	}
	r, e = s.CreateResult(ctx, "t", "s", WAV, 48)
	if e != nil {
		t.Fatal(e)
	}
	r.Write(wav()[:47])
	if _, e = s.PublishResult(ctx, "t", r); e == nil {
		t.Fatal("partial result")
	}
	r, e = s.CreateResult(ctx, "t", "s", WAV, 48)
	if e != nil {
		t.Fatal(e)
	}
	r.Write(wav())
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.PublishResult(c, "t", r); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestRejectLinkedRootAndOutput(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "linked")
	if e := os.Symlink(target, link); e != nil {
		t.Skipf("symlink privilege unavailable: %v", e)
	}
	if s, e := New(link); e == nil {
		s.Close()
		t.Fatal("linked root")
	}
	s, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.CreateResult(context.Background(), "t", "s", WAV, 48)
	if e != nil {
		t.Fatal(e)
	}
	r.Write(wav())
	// Replace the unpublished entry with an external link. Derive must reject it,
	// and cleanup must unlink only this entry, never touch the external target.
	external := filepath.Join(target, "keep.wav")
	if e = os.WriteFile(external, wav(), 0600); e != nil {
		t.Fatal(e)
	}
	r.file.Close()
	if e = s.dir.Remove(r.id); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(external, filepath.Join(s.scope.Path(), r.id)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PublishResult(context.Background(), "t", r); e == nil {
		t.Fatal("linked output")
	}
	b, e := os.ReadFile(external)
	if e != nil || !bytes.Equal(b, wav()) {
		t.Fatal("external file changed")
	}
}
func TestLimitsCannotExceedProduction(t *testing.T) {
	for _, l := range []limits{{hard.wav + 1, 1, 1, 1, 1}, {1, hard.image + 1, 1, 1, 1}, {1, 1, hard.json + 1, 1, 1}, {1, 1, 1, hard.total + 1, 1}, {1, 1, 1, 1, 33}} {
		if s, e := newWithLimits(t.TempDir(), l); e == nil {
			s.Close()
			t.Fatal("hard limit overridden")
		}
	}
}
