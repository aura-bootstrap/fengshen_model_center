package highlightclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

const maxImageBytes = 8 << 20
const maxWAVBytes = 1 << 30

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
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, 0, errMedia
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, 0, errMedia
	}
	opened, e := f.Stat()
	if e != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		f.Close()
		return nil, 0, errMedia
	}
	return f, opened.Size(), nil
}
func sameSource(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	a, e := filepath.Abs(a)
	if e != nil {
		return false
	}
	b, e = filepath.Abs(b)
	if e != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func (c *Client) uploadPrepared(ctx context.Context, ep domain.Episode, p audio.PreparedAudio) (integration.RuntimeAsset, error) {
	if e := ctx.Err(); e != nil {
		return integration.RuntimeAsset{}, e
	}
	if p.Validate() != nil || p.EpisodeNumber != ep.EpisodeNumber || !sameSource(p.SourceVideoPath, ep.VideoPath) {
		return integration.RuntimeAsset{}, errMedia
	}
	f, size, e := openRegular(p.WAVPath, maxWAVBytes)
	if e != nil {
		return integration.RuntimeAsset{}, e
	}
	defer f.Close()
	// Validate the same open file that is uploaded; hash the entire WAV, not PCM.
	sum := sha256.New()
	r := io.TeeReader(contextReader{ctx, f}, sum)
	if e = validateWAV(r, size, p); e != nil {
		if ctx.Err() != nil {
			return integration.RuntimeAsset{}, ctx.Err()
		}
		return integration.RuntimeAsset{}, e
	}
	if hex.EncodeToString(sum.Sum(nil)) != p.ContentSHA256 {
		return integration.RuntimeAsset{}, errMedia
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return integration.RuntimeAsset{}, errMedia
	}
	uploaded := sha256.New()
	a, e := c.transport.Upload(ctx, "audio/wav", size, io.TeeReader(contextReader{ctx, f}, uploaded))
	if e != nil {
		return integration.RuntimeAsset{}, e
	}
	m := a.Metadata
	if a.ID == "" || m.Kind != "wav" || m.Bytes != itoa(size) || m.SHA256 != p.ContentSHA256 || hex.EncodeToString(uploaded.Sum(nil)) != p.ContentSHA256 || m.SampleRate != uint32(p.Format.SampleRate) || m.Channels != uint16(p.Format.Channels) || !domain.Finite(m.DurationSeconds) || math.Abs(m.DurationSeconds-float64(p.SampleCount)/float64(p.Format.SampleRate)) > 1e-9 || m.Width != 0 || m.Height != 0 {
		return integration.RuntimeAsset{}, errMedia
	}
	return a, nil
}
func validateWAV(r io.Reader, size int64, p audio.PreparedAudio) error {
	var header [12]byte
	if _, e := io.ReadFull(r, header[:]); e != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(header[4:]))+8 != size {
		return errMedia
	}
	haveFmt, haveData := false, false
	var count int64
	for pos := int64(12); pos < size; {
		var chunk [8]byte
		if size-pos < 8 {
			return errMedia
		}
		if _, e := io.ReadFull(r, chunk[:]); e != nil {
			return e
		}
		n := int64(binary.LittleEndian.Uint32(chunk[4:]))
		pos += 8
		if n > size-pos {
			return errMedia
		}
		switch string(chunk[:4]) {
		case "fmt ":
			if haveFmt || haveData || n != 16 && n != 18 {
				return errMedia
			}
			var v [18]byte
			if _, e := io.ReadFull(r, v[:n]); e != nil {
				return e
			}
			if binary.LittleEndian.Uint16(v[:]) != 1 || binary.LittleEndian.Uint16(v[2:]) != uint16(p.Format.Channels) || binary.LittleEndian.Uint32(v[4:]) != uint32(p.Format.SampleRate) || binary.LittleEndian.Uint32(v[8:]) != uint32(p.Format.SampleRate*2) || binary.LittleEndian.Uint16(v[12:]) != 2 || binary.LittleEndian.Uint16(v[14:]) != 16 || n == 18 && binary.LittleEndian.Uint16(v[16:]) != 0 {
				return errMedia
			}
			haveFmt = true
		case "data":
			if !haveFmt || haveData || n == 0 || n%2 != 0 {
				return errMedia
			}
			haveData = true
			count = n / 2
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return e
			}
		case "JUNK", "LIST", "bext":
			if _, e := io.CopyN(io.Discard, r, n); e != nil {
				return e
			}
		default:
			return errMedia
		}
		pos += n
		if n%2 != 0 {
			var pad [1]byte
			if pos >= size {
				return errMedia
			}
			if _, e := io.ReadFull(r, pad[:]); e != nil || pad[0] != 0 {
				return errMedia
			}
			pos++
		}
	}
	if !haveFmt || !haveData || count != p.SampleCount || math.Abs(float64(count)/float64(p.Format.SampleRate)-p.DurationSec) > 1/float64(p.Format.SampleRate) {
		return errMedia
	}
	var extra [1]byte
	n, e := r.Read(extra[:])
	if n != 0 || e != io.EOF {
		return errMedia
	}
	return nil
}
func (c *Client) uploadImage(ctx context.Context, in scoring.CandidateInput) (integration.RuntimeAsset, error) {
	if e := ctx.Err(); e != nil {
		return integration.RuntimeAsset{}, e
	}
	if (in.Image == nil) == (in.ContactSheetPath == "") {
		return integration.RuntimeAsset{}, errMedia
	}
	var raw []byte
	declared := ""
	if in.Image != nil {
		declared = in.Image.MIMEType
		if declared != "image/png" && declared != "image/jpeg" {
			return integration.RuntimeAsset{}, errMedia
		}
		s := in.Image.DataBase64
		if len(s) == 0 || len(s) > base64.StdEncoding.EncodedLen(maxImageBytes) || strings.ContainsAny(s, "\r\n") {
			return integration.RuntimeAsset{}, errMedia
		}
		var e error
		raw, e = base64.StdEncoding.Strict().DecodeString(s)
		if e != nil || len(raw) == 0 || len(raw) > maxImageBytes {
			return integration.RuntimeAsset{}, errMedia
		}
	} else {
		f, _, e := openRegular(in.ContactSheetPath, maxImageBytes)
		if e != nil {
			return integration.RuntimeAsset{}, e
		}
		defer f.Close()
		raw, e = io.ReadAll(io.LimitReader(contextReader{ctx, f}, maxImageBytes+1))
		if e != nil {
			return integration.RuntimeAsset{}, e
		}
		if len(raw) == 0 || len(raw) > maxImageBytes {
			return integration.RuntimeAsset{}, errMedia
		}
	}
	config, format, e := image.DecodeConfig(bytes.NewReader(raw))
	if e != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return integration.RuntimeAsset{}, errMedia
	}
	typ := "image/" + format
	if typ != "image/png" && typ != "image/jpeg" || declared != "" && typ != declared {
		return integration.RuntimeAsset{}, errMedia
	}
	decoded, decodedFormat, e := image.Decode(bytes.NewReader(raw))
	if e != nil || decodedFormat != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return integration.RuntimeAsset{}, errMedia
	}
	sum := sha256.Sum256(raw)
	a, e := c.transport.Upload(ctx, typ, int64(len(raw)), bytes.NewReader(raw))
	if e != nil {
		return integration.RuntimeAsset{}, e
	}
	m := a.Metadata
	if a.ID == "" || m.Kind != "image" || m.Bytes != itoa(int64(len(raw))) || m.SHA256 != hex.EncodeToString(sum[:]) || m.Width != config.Width || m.Height != config.Height || m.SampleRate != 0 || m.Channels != 0 || m.DurationSeconds != 0 {
		return integration.RuntimeAsset{}, errMedia
	}
	return a, nil
}
