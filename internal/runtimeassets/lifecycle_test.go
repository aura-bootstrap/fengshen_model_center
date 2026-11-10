package runtimeassets

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestReleaseDuringUploadAndOpenReader(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	a, e := s.Upload(ctx, "t", "reader-session", WAV, 48, bytes.NewReader(wav()))
	if e != nil {
		t.Fatal(e)
	}
	reader, _, e := s.Open(ctx, "t", a.ID())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ReleaseSession("reader-session"); e != nil {
		t.Fatal(e)
	}
	if _, e = io.ReadAll(reader); e == nil {
		t.Fatal("released reader remains readable")
	}
	g := &gateReader{entered: make(chan struct{}), resume: make(chan struct{}), r: bytes.NewReader(wav())}
	done := make(chan error, 1)
	go func() { _, e := s.Upload(ctx, "t", "s", WAV, 48, g); done <- e }()
	<-g.entered
	if e = s.ReleaseSession("s"); e != nil {
		t.Fatal(e)
	}
	close(g.resume)
	if e = <-done; e == nil {
		t.Fatal("released upload published")
	}
	if _, e = s.Upload(ctx, "t", "s", WAV, 48, bytes.NewReader(wav())); e == nil {
		t.Fatal("released session resurrected")
	}
}
func TestOverflowCannotPublishPrefix(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	r, e := s.CreateResult(ctx, "t", "s", WAV, 48)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Write(wav()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Write([]byte{0}); e == nil {
		t.Fatal("overflow")
	}
	if _, e = s.PublishResult(ctx, "t", r); e == nil {
		t.Fatal("overflow prefix published")
	}
}
