package runtimeassets

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func wav() []byte {
	b := make([]byte, 48)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 40)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 4)
	return b
}
func TestRoundTrip(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	b := wav()
	a, e := s.Upload(ctx, "task", "session", WAV, int64(len(b)), bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	f, m, e := s.Open(ctx, "task", a.ID())
	if e != nil {
		t.Fatal(e)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, b) || m.Bytes != "48" || m.SampleRate != 16000 || m.Channels != 1 || len(m.SHA256) != 64 {
		t.Fatalf("bad metadata: %+v", m)
	}
	if _, _, e = s.Open(ctx, "other", a.ID()); e == nil {
		t.Fatal("cross task")
	}
	r, e := s.CreateResult(ctx, "task", "session", WAV, 48)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Write(b); e != nil {
		t.Fatal(e)
	}
	out, e := s.PublishResult(ctx, "task", r)
	if e != nil {
		t.Fatal(e)
	}
	d, _, e := s.Download(ctx, "task", out.ID())
	if e != nil {
		t.Fatal(e)
	}
	d.Close()
	if _, _, e = s.Download(ctx, "other", out.ID()); e == nil {
		t.Fatal("cross result")
	}
	if _, _, e = s.Download(ctx, "task", a.ID()); e == nil {
		t.Fatal("input download")
	}
	if e = s.ReleaseSession("session"); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Open(ctx, "task", a.ID()); e == nil {
		t.Fatal("released")
	}
}
func TestRejectAndRollback(t *testing.T) {
	s, e := newWithLimits(t.TempDir(), limits{wav: 64, image: 64, json: 64, total: 64, items: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	b := wav()
	ctx := context.Background()
	for _, bad := range [][]byte{[]byte("RIFFfake"), b[:47], append(append([]byte{}, b...), 0)} {
		if _, e = s.Upload(ctx, "t", "s", WAV, 48, bytes.NewReader(bad)); e == nil {
			t.Fatal("accepted corrupt")
		}
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.Upload(c, "t", "s", WAV, 48, bytes.NewReader(b)); e == nil {
		t.Fatal("cancelled")
	}
	if _, e = s.Upload(ctx, "t", "s", WAV, 48, bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upload(ctx, "t", "s", WAV, 48, bytes.NewReader(b)); e == nil {
		t.Fatal("quota")
	}
	if _, e = s.PublishResult(ctx, "t", &ResultFile{}); e == nil {
		t.Fatal("unowned result")
	}
	if _, _, e = s.Download(ctx, "t", "../../go.mod"); e == nil {
		t.Fatal("path injection")
	}
}
func TestOwnDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep")
	if e := os.WriteFile(sentinel, []byte("safe"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(sentinel); e != nil {
		t.Fatal(e)
	}
}
