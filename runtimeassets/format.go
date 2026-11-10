package runtimeassets

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
)

// inspect validates the complete file before it can become visible. WAV accepts
// classic little-endian integer PCM only (no float, RF64, compressed or extensible).
func inspect(ctx context.Context, f *os.File, k Kind, n int64) (Metadata, error) {
	m := Metadata{Kind: k, Bytes: byteString(n)}
	var e error
	switch k {
	case WAV:
		e = inspectWAV(ctx, f, n, &m)
	case Image:
		var c image.Config
		c, _, e = image.DecodeConfig(contextReader{ctx, io.NewSectionReader(f, 0, n)})
		if e == nil && (c.Width < 1 || c.Height < 1 || c.Width > 4096 || c.Height > 4096) {
			e = ErrInvalid
		}
		if e == nil {
			var img image.Image
			img, _, e = image.Decode(contextReader{ctx, io.NewSectionReader(f, 0, n)})
			if e == nil {
				b := img.Bounds()
				if b.Dx() != c.Width || b.Dy() != c.Height {
					e = ErrInvalid
				} else {
					m.Width = c.Width
					m.Height = c.Height
				}
			}
		}
	case JSON:
		// JSON is bounded to 4 MiB before reaching here. Reject trailing documents.
		var b []byte
		b, e = io.ReadAll(contextReader{ctx, io.NewSectionReader(f, 0, n)})
		if e == nil && !json.Valid(b) {
			e = ErrInvalid
		}
	default:
		e = ErrInvalid
	}
	if e != nil {
		if ctx.Err() != nil {
			return Metadata{}, ctx.Err()
		}
		return Metadata{}, ErrInvalid
	}
	m.SHA256, e = digest(ctx, f, n)
	return m, e
}
func inspectWAV(ctx context.Context, f *os.File, n int64, m *Metadata) error {
	if n < 44 {
		return ErrInvalid
	}
	var header [12]byte
	if _, e := f.ReadAt(header[:], 0); e != nil {
		return ErrInvalid
	}
	if !bytes.Equal(header[:4], []byte("RIFF")) || !bytes.Equal(header[8:], []byte("WAVE")) || int64(binary.LittleEndian.Uint32(header[4:]))+8 != n {
		return ErrInvalid
	}
	var haveFmt, haveData bool
	var align uint16
	var rate uint32
	var data int64
	for pos := int64(12); pos < n; {
		if e := ctx.Err(); e != nil {
			return e
		}
		if n-pos < 8 {
			return ErrInvalid
		}
		var h [8]byte
		if _, e := f.ReadAt(h[:], pos); e != nil {
			return ErrInvalid
		}
		size := int64(binary.LittleEndian.Uint32(h[4:]))
		pos += 8
		if size > n-pos {
			return ErrInvalid
		}
		switch string(h[:4]) {
		case "fmt ":
			if haveFmt || haveData || (size != 16 && size != 18) {
				return ErrInvalid
			}
			haveFmt = true
			var b [18]byte
			if _, e := f.ReadAt(b[:size], pos); e != nil {
				return ErrInvalid
			}
			if binary.LittleEndian.Uint16(b[:]) != 1 || (size == 18 && binary.LittleEndian.Uint16(b[16:]) != 0) {
				return ErrInvalid
			}
			channels := binary.LittleEndian.Uint16(b[2:])
			sampleRate := binary.LittleEndian.Uint32(b[4:])
			byteRate := binary.LittleEndian.Uint32(b[8:])
			align = binary.LittleEndian.Uint16(b[12:])
			bits := binary.LittleEndian.Uint16(b[14:])
			if channels < 1 || channels > 8 || sampleRate < 8000 || sampleRate > 192000 || (bits != 8 && bits != 16 && bits != 24 && bits != 32) || align != channels*(bits/8) || byteRate != sampleRate*uint32(align) {
				return ErrInvalid
			}
			m.Channels = channels
			m.SampleRate = sampleRate
			rate = byteRate
		case "data":
			if !haveFmt || haveData || size == 0 || size%int64(align) != 0 {
				return ErrInvalid
			}
			haveData = true
			data = size
		default:
			// Only harmless standard padding/metadata chunks are accepted.
			switch string(h[:4]) {
			case "JUNK", "LIST", "bext":
			default:
				return ErrInvalid
			}
		}
		pos += size
		if size&1 != 0 {
			if pos >= n {
				return ErrInvalid
			}
			var pad [1]byte
			if _, e := f.ReadAt(pad[:], pos); e != nil || pad[0] != 0 {
				return ErrInvalid
			}
			pos++
		}
	}
	if !haveFmt || !haveData {
		return ErrInvalid
	}
	m.DurationSeconds = float64(data) / float64(rate)
	return nil
}
