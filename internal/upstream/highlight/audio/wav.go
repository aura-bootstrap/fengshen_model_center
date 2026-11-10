package audio

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const DefaultMaxWAVBytes int64 = 512 << 20

// ReadOptions bounds both input bytes and the accepted PCM format.
type ReadOptions struct {
	MaxBytes       int64
	ExpectedFormat PCMFormat
}

// PCM is a bounded, decoded analysis buffer. It is local-only just like its
// source WAV and is never part of audio_features_v1.
type PCM struct {
	Format        PCMFormat `json:"-"`
	Samples       []int16   `json:"-"`
	DurationSec   float64   `json:"-"`
	ContentSHA256 string    `json:"-"`
	FileSize      int64     `json:"-"`
}

func (PCM) MarshalJSON() ([]byte, error) { return nil, ErrLocalMediaSerialization }

// ReadWAV reads one RIFF/WAVE file with a hard byte limit. At most one options
// value may be supplied; omitted options select the fixed analysis format.
func ReadWAV(ctx context.Context, path string, options ...ReadOptions) (PCM, error) {
	if len(options) > 1 {
		return PCM{}, fmt.Errorf("%w: multiple WAV read options", ErrInvalidRequest)
	}
	option := ReadOptions{}
	if len(options) == 1 {
		option = options[0]
	}
	if option.MaxBytes == 0 {
		option.MaxBytes = DefaultMaxWAVBytes
	}
	if option.MaxBytes < 0 {
		return PCM{}, fmt.Errorf("%w: negative WAV byte limit", ErrInvalidRequest)
	}
	format, err := option.ExpectedFormat.normalized()
	if err != nil {
		return PCM{}, err
	}
	if err := ctx.Err(); err != nil {
		return PCM{}, err
	}

	file, err := os.Open(path)
	if err != nil {
		return PCM{}, fmt.Errorf("%w: %w", ErrWAVRead, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return PCM{}, fmt.Errorf("%w: %w", ErrWAVRead, err)
	}
	if !info.Mode().IsRegular() {
		return PCM{}, fmt.Errorf("%w: WAV is not a regular file", ErrWAVFormat)
	}
	if info.Size() > option.MaxBytes {
		return PCM{}, fmt.Errorf("%w: file size %d exceeds %d", ErrWAVTooLarge, info.Size(), option.MaxBytes)
	}
	data, err := readAllContext(ctx, file, option.MaxBytes)
	if err != nil {
		return PCM{}, err
	}
	return decodeWAV(ctx, data, format)
}

func readAllContext(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || maxBytes == 1<<63-1 {
		return nil, fmt.Errorf("%w: invalid read limit", ErrInvalidRequest)
	}
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: reader}, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %w", ErrWAVRead, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: streamed input exceeds %d", ErrWAVTooLarge, maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return count, ctxErr
	}
	return count, err
}

func decodeWAV(ctx context.Context, data []byte, expected PCMFormat) (PCM, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return PCM{}, fmt.Errorf("%w: missing RIFF/WAVE header", ErrWAVFormat)
	}
	riffSize := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if riffSize+8 != uint64(len(data)) {
		return PCM{}, fmt.Errorf("%w: RIFF size is truncated or has trailing data", ErrWAVFormat)
	}

	var (
		formatSeen bool
		dataSeen   bool
		pcmData    []byte
	)
	for offset := 12; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return PCM{}, err
		}
		if len(data)-offset < 8 {
			return PCM{}, fmt.Errorf("%w: truncated chunk header", ErrWAVFormat)
		}
		chunkID := string(data[offset : offset+4])
		chunkSize := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		payloadStart := offset + 8
		payloadEnd := uint64(payloadStart) + chunkSize
		paddedEnd := payloadEnd + chunkSize%2
		if payloadEnd > uint64(len(data)) || paddedEnd > uint64(len(data)) {
			return PCM{}, fmt.Errorf("%w: truncated %s chunk", ErrWAVFormat, chunkID)
		}
		payload := data[payloadStart:int(payloadEnd)]
		switch chunkID {
		case "fmt ":
			if formatSeen || dataSeen || len(payload) < 16 {
				return PCM{}, fmt.Errorf("%w: missing, duplicate, or misplaced fmt chunk", ErrWAVFormat)
			}
			formatSeen = true
			encoding := binary.LittleEndian.Uint16(payload[0:2])
			format := PCMFormat{
				Channels:      int(binary.LittleEndian.Uint16(payload[2:4])),
				SampleRate:    int(binary.LittleEndian.Uint32(payload[4:8])),
				BitsPerSample: int(binary.LittleEndian.Uint16(payload[14:16])),
			}
			blockAlign := binary.LittleEndian.Uint16(payload[12:14])
			byteRate := binary.LittleEndian.Uint32(payload[8:12])
			expectedBlockAlign := uint16(expected.Channels * expected.BitsPerSample / 8)
			expectedByteRate := uint32(expected.SampleRate * expected.Channels * expected.BitsPerSample / 8)
			if encoding != 1 || format != expected || blockAlign != expectedBlockAlign || byteRate != expectedByteRate {
				return PCM{}, fmt.Errorf("%w: WAV is not 16 kHz mono PCM signed 16-bit", ErrWAVFormat)
			}
		case "data":
			if !formatSeen || dataSeen {
				return PCM{}, fmt.Errorf("%w: missing or duplicate data chunk", ErrWAVFormat)
			}
			dataSeen = true
			pcmData = payload
		}
		offset = int(paddedEnd)
	}
	if !formatSeen || !dataSeen {
		return PCM{}, fmt.Errorf("%w: fmt and data chunks are required", ErrWAVFormat)
	}
	if len(pcmData)%2 != 0 {
		return PCM{}, fmt.Errorf("%w: 16-bit PCM data has an odd byte count", ErrWAVFormat)
	}

	samples := make([]int16, len(pcmData)/2)
	for index := range samples {
		if index%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return PCM{}, err
			}
		}
		samples[index] = int16(binary.LittleEndian.Uint16(pcmData[index*2 : index*2+2]))
	}
	if err := ctx.Err(); err != nil {
		return PCM{}, err
	}
	digest := sha256.Sum256(data)
	return PCM{
		Format:        expected,
		Samples:       samples,
		DurationSec:   float64(len(samples)) / float64(expected.SampleRate),
		ContentSHA256: hex.EncodeToString(digest[:]),
		FileSize:      int64(len(data)),
	}, nil
}

// Ensure local PCM keeps the same fail-closed behavior behind interface fields.
var _ json.Marshaler = PCM{}
