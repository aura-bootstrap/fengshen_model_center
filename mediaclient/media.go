package mediaclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func openRegular(path string, limit int64) (*os.File, int64, error) {
	s, e := os.Lstat(path)
	if e != nil || !s.Mode().IsRegular() || s.Size() <= 0 || s.Size() > limit {
		return nil, 0, errMedia
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, 0, errMedia
	}
	v, e := f.Stat()
	if e != nil || !v.Mode().IsRegular() || !os.SameFile(s, v) || v.Size() != s.Size() {
		f.Close()
		return nil, 0, errMedia
	}
	return f, v.Size(), nil
}

type wavProperties struct {
	rate, channels int
	frames         int64
}

func (p wavProperties) duration() float64 { return float64(p.frames) / float64(p.rate) }

// Match the center's PCM RIFF subset, restricted to its timbre input channels.
func readWAV(r io.Reader, size int64) (p wavProperties, err error) {
	fail := func() (wavProperties, error) { return wavProperties{}, errMedia }
	var h [12]byte
	if size < 44 || size > 1<<30 {
		return fail()
	}
	if _, e := io.ReadFull(r, h[:]); e != nil || string(h[:4]) != "RIFF" || string(h[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(h[4:]))+8 != size {
		return fail()
	}
	haveFmt, haveData := false, false
	var align int64
	for pos := int64(12); pos < size; {
		var ch [8]byte
		if size-pos < 8 {
			return fail()
		}
		if _, e := io.ReadFull(r, ch[:]); e != nil {
			return fail()
		}
		n := int64(binary.LittleEndian.Uint32(ch[4:]))
		pos += 8
		if n > size-pos {
			return fail()
		}
		switch string(ch[:4]) {
		case "fmt ":
			if haveFmt || haveData || n != 16 && n != 18 {
				return fail()
			}
			var b [18]byte
			if _, e := io.ReadFull(r, b[:n]); e != nil {
				return fail()
			}
			p.channels = int(binary.LittleEndian.Uint16(b[2:]))
			p.rate = int(binary.LittleEndian.Uint32(b[4:]))
			bits := int(binary.LittleEndian.Uint16(b[14:]))
			align = int64(binary.LittleEndian.Uint16(b[12:]))
			if binary.LittleEndian.Uint16(b[:]) != 1 || p.channels < 1 || p.channels > 2 || p.rate < 8000 || p.rate > 192000 || bits != 8 && bits != 16 && bits != 24 && bits != 32 || align != int64(p.channels*bits/8) || int64(binary.LittleEndian.Uint32(b[8:])) != int64(p.rate)*align || n == 18 && binary.LittleEndian.Uint16(b[16:]) != 0 {
				return fail()
			}
			haveFmt = true
		case "data":
			if !haveFmt || haveData || n == 0 || align == 0 || n%align != 0 {
				return fail()
			}
			p.frames = n / align
			haveData = true
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return fail()
			}
		case "JUNK", "LIST", "bext":
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return fail()
			}
		default:
			return fail()
		}
		pos += n
		if n&1 != 0 {
			var pad [1]byte
			if pos >= size {
				return fail()
			}
			if _, e := io.ReadFull(r, pad[:]); e != nil || pad[0] != 0 {
				return fail()
			}
			pos++
		}
	}
	var extra [1]byte
	n, e := r.Read(extra[:])
	if !haveFmt || !haveData || p.duration() > 86400 || n != 0 || e != io.EOF {
		return fail()
	}
	return p, nil
}
func (c *Client) uploadWAV(ctx context.Context, path string) (integration.RuntimeAsset, wavProperties, error) {
	var empty integration.RuntimeAsset
	var p wavProperties
	if e := ctx.Err(); e != nil {
		return empty, p, e
	}
	f, n, e := openRegular(path, 1<<30)
	if e != nil {
		return empty, p, e
	}
	defer f.Close()
	h := sha256.New()
	p, e = readWAV(io.TeeReader(contextReader{ctx, f}, h), n)
	if e != nil {
		if ctx.Err() != nil {
			return empty, p, ctx.Err()
		}
		return empty, p, e
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return empty, p, errMedia
	}
	uploaded := sha256.New()
	count := &countReader{r: io.TeeReader(contextReader{ctx, f}, uploaded)}
	a, e := c.transport.Upload(ctx, "audio/wav", n, count)
	if e != nil {
		return empty, p, e
	}
	var extra [1]byte
	got, end := f.Read(extra[:])
	m := a.Metadata
	if count.n != n || got != 0 || end != io.EOF || hex.EncodeToString(uploaded.Sum(nil)) != sum || a.ID == "" || m.Kind != "wav" || m.Bytes != strconv.FormatInt(n, 10) || m.SHA256 != sum || m.SampleRate != uint32(p.rate) || m.Channels != uint16(p.channels) || !finite(m.DurationSeconds) || math.Abs(m.DurationSeconds-p.duration()) > 1e-9 || m.Width != 0 || m.Height != 0 {
		return empty, p, errMedia
	}
	return a, p, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (r *countReader) Read(p []byte) (int, error) { n, e := r.r.Read(p); r.n += int64(n); return n, e }
func finite(v float64) bool                       { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func (c *Client) uploadImage(ctx context.Context, path, expectedSHA string) (integration.RuntimeAsset, error) {
	var empty integration.RuntimeAsset
	if e := ctx.Err(); e != nil {
		return empty, e
	}
	f, n, e := openRegular(path, 8<<20)
	if e != nil {
		return empty, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(contextReader{ctx, f}, n+1))
	if e != nil {
		return empty, e
	}
	if int64(len(raw)) != n {
		return empty, errMedia
	}
	expected, e := hex.DecodeString(expectedSHA)
	h := sha256.Sum256(raw)
	if e != nil || len(expected) != sha256.Size || !bytes.Equal(h[:], expected) {
		return empty, errMedia
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(raw))
	if e != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || format != "png" && format != "jpeg" {
		return empty, errMedia
	}
	decoded, kind, e := image.Decode(bytes.NewReader(raw))
	if e != nil || kind != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return empty, errMedia
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	sum := hex.EncodeToString(h[:])
	a, e := c.transport.Upload(ctx, "image/"+format, n, bytes.NewReader(raw))
	if e != nil {
		return empty, e
	}
	m := a.Metadata
	if a.ID == "" || m.Kind != "image" || m.Bytes != strconv.FormatInt(n, 10) || m.SHA256 != sum || m.Width != cfg.Width || m.Height != cfg.Height || m.SampleRate != 0 || m.Channels != 0 || m.DurationSeconds != 0 {
		return empty, errMedia
	}
	return a, nil
}

// Reserve all destinations before any upload. O_EXCL never truncates an existing
// file, including symlinks. Rollback removes only files this operation created.
func (c *Client) audio(ctx context.Context, scene, source, reference string, paths []string) (err error) {
	b, e := c.binding(scene)
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	files := make([]*os.File, 0, len(paths))
	infos := make([]os.FileInfo, 0, len(paths))
	committed := false
	defer func() {
		for i, f := range files {
			_ = f.Close()
			if !committed {
				current, e := os.Lstat(paths[i])
				if e == nil && os.SameFile(infos[i], current) {
					_ = os.Remove(paths[i])
				}
			}
		}
	}()
	for _, path := range paths {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return errors.New("center_output_create_failed")
		}
		s, e := f.Stat()
		if e != nil {
			f.Close()
			return errors.New("center_output_create_failed")
		}
		files = append(files, f)
		infos = append(infos, s)
	}
	a, p, e := c.uploadWAV(ctx, source)
	if e != nil {
		return e
	}
	body := integration.RuntimeBody{Component: b.Component}
	rate, channels := 44100, 2
	if scene == "timbre.convert" {
		ref, _, e := c.uploadWAV(ctx, reference)
		if e != nil {
			return e
		}
		body.Convert = &integration.RuntimeConvertInput{AssetID: a.ID, ReferenceAssetID: ref.ID, Pitch: 0}
		rate, channels = 22050, 1
	} else {
		body.Separate = &integration.RuntimeAssetInput{AssetID: a.ID}
	}
	err = c.run(ctx, b, body, func(j *job, r integration.RuntimeResult) error {
		if len(r.Media) != len(files) || r.Items != nil || r.Stream != nil || r.Events != nil || r.Face != "" {
			return errResult
		}
		seen := map[string]bool{}
		for i, m := range r.Media {
			n, e := strconv.ParseInt(m.Bytes, 10, 64)
			hash, eHash := hex.DecodeString(m.SHA256)
			if m.ID == "" || seen[m.ID] || e != nil || strconv.FormatInt(n, 10) != m.Bytes || n < 44 || n > 1<<30 || eHash != nil || len(hash) != sha256.Size || m.SampleRate != rate || m.Channels != channels || m.DurationMS <= 0 || m.DurationMS > 86400000 {
				return errResult
			}
			seen[m.ID] = true
			if e = c.transport.DownloadWAV(ctx, j, m.ID, files[i]); e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			s, e := files[i].Stat()
			if e != nil || s.Size() != n {
				return errMedia
			}
			if _, e = files[i].Seek(0, io.SeekStart); e != nil {
				return errMedia
			}
			h := sha256.New()
			out, e := readWAV(io.TeeReader(contextReader{ctx, files[i]}, h), n)
			if e != nil || hex.EncodeToString(h.Sum(nil)) != m.SHA256 || out.rate != rate || out.channels != channels || math.Abs(out.duration()*1000-float64(m.DurationMS)) > 1 || !durationWithin(out, p) {
				return errMedia
			}
		}
		for _, f := range files {
			if e := c.syncOutput(f); e != nil {
				return errors.New("center_output_write_failed")
			}
		}
		for _, f := range files {
			if e := f.Close(); e != nil {
				return errors.New("center_output_write_failed")
			}
		}
		for _, path := range paths {
			if e := c.persistOutput(path); e != nil {
				return errors.New("center_output_write_failed")
			}
		}
		// An unknown Ack must not delete validated, durably published outputs.
		committed = true
		return nil
	})
	return err
}

// Integer sample arithmetic makes the strict 100 ms boundary unambiguous.
func durationWithin(a, b wavProperties) bool {
	difference := a.frames*int64(b.rate) - b.frames*int64(a.rate)
	if difference < 0 {
		difference = -difference
	}
	return difference*10 < int64(a.rate)*int64(b.rate)
}
