package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Info is the subset of ffprobe metadata needed for safe source verification.
type Info struct {
	DurationSec      float64
	VideoDurationSec float64
	AudioDurationSec float64
	StartSec         float64
	HasVideo         bool
	HasAudio         bool
}

type probeDocument struct {
	Format struct {
		Duration string `json:"duration"`
		Start    string `json:"start_time"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		Duration  string `json:"duration"`
		Start     string `json:"start_time"`
	} `json:"streams"`
}

// Probe reads duration, streams and start timestamps with a bounded JSON
// response. Duration intentionally keeps its legacy command for compatibility.
func (c *FFmpegClient) Probe(ctx context.Context, path string) (Info, error) {
	if strings.TrimSpace(path) == "" {
		return Info{}, fmt.Errorf("%w: video path is required", ErrInvalidRequest)
	}
	stdout, _, err := c.commandRunner().Run(ctx,
		c.ffprobePath(), "-v", "error", "-show_entries",
		"format=duration,start_time:stream=codec_type,duration,start_time",
		"-of", "json", path,
	)
	if err != nil {
		return Info{}, fmt.Errorf("probe media metadata: %w", err)
	}
	var document probeDocument
	decoder := json.NewDecoder(bytes.NewBufferString(stdout))
	if err := decoder.Decode(&document); err != nil {
		return Info{}, fmt.Errorf("%w: invalid ffprobe JSON", ErrInvalidDuration)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Info{}, fmt.Errorf("%w: trailing ffprobe JSON", ErrInvalidDuration)
	}
	duration, err := strconv.ParseFloat(document.Format.Duration, 64)
	if err != nil || !validDuration(duration) {
		return Info{}, ErrInvalidDuration
	}
	info := Info{DurationSec: duration}
	info.StartSec, err = parseOptionalProbeFloat(document.Format.Start)
	if err != nil {
		return Info{}, fmt.Errorf("%w: invalid media start timestamp", ErrInvalidRequest)
	}
	videoDuration, audioDuration := 0.0, 0.0
	for _, stream := range document.Streams {
		value, parseErr := parseOptionalProbeFloat(stream.Duration)
		if parseErr != nil || value < 0 {
			return Info{}, fmt.Errorf("%w: invalid stream duration", ErrInvalidDuration)
		}
		switch stream.CodecType {
		case "video":
			info.HasVideo = true
			videoDuration = math.Max(videoDuration, value)
		case "audio":
			info.HasAudio = true
			audioDuration = math.Max(audioDuration, value)
		}
	}
	if !info.HasVideo {
		return Info{}, fmt.Errorf("%w: no video stream", ErrInvalidRequest)
	}
	info.VideoDurationSec = videoDuration
	info.AudioDurationSec = audioDuration
	return info, nil
}

func parseOptionalProbeFloat(raw string) (float64, error) {
	text := strings.TrimSpace(raw)
	if text == "" || strings.EqualFold(text, "N/A") {
		return 0, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, ErrInvalidDuration
	}
	return value, nil
}

// VerifySource rejects missing video, non-zero starts, invalid duration and
// excessive audio/video drift before a clip enters the traditional pipeline.
func (c *FFmpegClient) VerifySource(ctx context.Context, path string, expectedDuration float64) (Info, error) {
	info, err := c.Probe(ctx, path)
	if err != nil {
		return Info{}, err
	}
	if !validDuration(expectedDuration) || math.Abs(info.DurationSec-expectedDuration) > 0.75 {
		return Info{}, fmt.Errorf("%w: exported duration differs from requested range", ErrInvalidDuration)
	}
	if math.Abs(info.StartSec) > 0.25 {
		return Info{}, fmt.Errorf("%w: exported start timestamp is not normalized", ErrInvalidRequest)
	}
	if info.HasAudio && info.VideoDurationSec > 0 && info.AudioDurationSec > 0 && math.Abs(info.VideoDurationSec-info.AudioDurationSec) > 0.25 {
		return Info{}, fmt.Errorf("%w: audio/video duration drift exceeds 250ms", ErrInvalidDuration)
	}
	return info, nil
}
