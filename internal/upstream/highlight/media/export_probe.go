package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ErrInvalidExport identifies an export that could not pass the output gate.
var ErrInvalidExport = errors.New("highlight_output_duration_invalid")

const exportProbeMaxBytes = 64 << 10

// ExportInfo preserves measured timestamps, not a pass/fail flag. On timing
// policy failure, VerifyExport returns the measurements alongside the error.
// FPS uses avg_frame_rate; r_frame_rate does not replace missing/invalid data.
type ExportInfo struct {
	ContainerStartSec    float64
	ContainerDurationSec float64
	VideoStartSec        float64
	VideoDurationSec     float64
	AudioStartSec        float64
	AudioDurationSec     float64
	FPS                  float64
	SampleRate           int
	ToleranceSec         float64
}

type exportProbeTiming struct {
	Start    string `json:"start_time"`
	Duration string `json:"duration"`
}

type exportProbeDocument struct {
	Format  exportProbeTiming `json:"format"`
	Streams []struct {
		exportProbeTiming
		CodecType    string `json:"codec_type"`
		CodecName    string `json:"codec_name"`
		AvgFrameRate string `json:"avg_frame_rate"`
		RFrameRate   string `json:"r_frame_rate"`
		SampleRate   string `json:"sample_rate"`
	} `json:"streams"`
}

// VerifyExport checks a final H.264/AAC export for an integer target in [1,600].
// It is separate from source probing, whose timing tolerances are looser.
func (c *FFmpegClient) VerifyExport(ctx context.Context, path string, expectedSeconds int) (ExportInfo, error) {
	var info ExportInfo
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || expectedSeconds < 1 || expectedSeconds > 600 {
		return info, fmt.Errorf("%w: path and integer target in [1,600] are required", ErrInvalidExport)
	}
	if err := ctx.Err(); err != nil {
		return info, fmt.Errorf("%w: %w", ErrInvalidExport, err)
	}
	stdout, _, runErr := c.commandRunner().Run(ctx,
		c.ffprobePath(), "-v", "error", "-show_entries",
		"format=duration,start_time:stream=codec_type,codec_name,duration,start_time,avg_frame_rate,r_frame_rate,sample_rate",
		"-of", "json", path,
	)
	if err := errors.Join(runErr, ctx.Err()); err != nil {
		return info, fmt.Errorf("%w: probe export: %w", ErrInvalidExport, err)
	}
	if len(stdout) > exportProbeMaxBytes {
		return info, fmt.Errorf("%w: ffprobe output exceeds %d bytes", ErrInvalidExport, exportProbeMaxBytes)
	}
	var document exportProbeDocument
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&document); err != nil {
		return info, fmt.Errorf("%w: invalid ffprobe JSON: %w", ErrInvalidExport, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return info, fmt.Errorf("%w: trailing ffprobe JSON", ErrInvalidExport)
	}
	if len(document.Streams) != 2 {
		return info, fmt.Errorf("%w: exactly one video and one AAC audio stream are required", ErrInvalidExport)
	}
	videoIndex, audioIndex := -1, -1
	for i, stream := range document.Streams {
		switch {
		case stream.CodecType == "video" && stream.CodecName == "h264" && videoIndex == -1:
			videoIndex = i
		case stream.CodecType == "audio" && stream.CodecName == "aac" && audioIndex == -1:
			audioIndex = i
		default:
			return info, fmt.Errorf("%w: expected H.264 video and AAC audio", ErrInvalidExport)
		}
	}
	if videoIndex == -1 || audioIndex == -1 {
		return info, fmt.Errorf("%w: missing video or audio stream", ErrInvalidExport)
	}
	video, audio := document.Streams[videoIndex], document.Streams[audioIndex]
	frame, err := exportVideoFrame(video.AvgFrameRate)
	if err != nil {
		return info, err
	}
	sampleRate, err := strconv.ParseInt(audio.SampleRate, 10, 32)
	if err != nil || sampleRate <= 0 {
		return info, fmt.Errorf("%w: invalid audio sample_rate", ErrInvalidExport)
	}
	info.FPS, _ = new(big.Rat).Inv(frame).Float64()
	info.SampleRate = int(sampleRate)
	aacFrame := big.NewRat(1024, sampleRate)
	tolerance := new(big.Rat).Add(frame, aacFrame)
	info.ToleranceSec, _ = tolerance.Float64()

	var starts, durations [3]*big.Rat
	for i, timing := range []exportProbeTiming{document.Format, video.exportProbeTiming, audio.exportProbeTiming} {
		starts[i], err = exportTimestamp(timing.Start, false)
		if err != nil {
			return info, fmt.Errorf("%w: track %d start_time: %w", ErrInvalidExport, i, err)
		}
		durations[i], err = exportTimestamp(timing.Duration, true)
		if err != nil {
			return info, fmt.Errorf("%w: track %d duration: %w", ErrInvalidExport, i, err)
		}
	}
	info.ContainerStartSec, _ = starts[0].Float64()
	info.VideoStartSec, _ = starts[1].Float64()
	info.AudioStartSec, _ = starts[2].Float64()
	info.ContainerDurationSec, _ = durations[0].Float64()
	info.VideoDurationSec, _ = durations[1].Float64()
	info.AudioDurationSec, _ = durations[2].Float64()

	target := big.NewRat(int64(expectedSeconds), 1)
	for i, maxStart := range []*big.Rat{tolerance, frame, aacFrame} {
		end := new(big.Rat).Add(starts[i], durations[i])
		if starts[i].Cmp(maxStart) > 0 || exportTimeDifference(durations[i], target).Cmp(tolerance) > 0 || exportTimeDifference(end, target).Cmp(tolerance) > 0 {
			return info, fmt.Errorf("%w: track %d start, duration or end exceeds frame tolerance", ErrInvalidExport, i)
		}
	}
	// Compare exact decimal timestamps: float subtraction can turn 0.1 into
	// 0.09999999999999787 and incorrectly admit the strict boundary.
	maxDrift := big.NewRat(1, 10)
	videoEnd := new(big.Rat).Add(starts[1], durations[1])
	audioEnd := new(big.Rat).Add(starts[2], durations[2])
	if exportTimeDifference(durations[1], durations[2]).Cmp(maxDrift) >= 0 || exportTimeDifference(videoEnd, audioEnd).Cmp(maxDrift) >= 0 {
		return info, fmt.Errorf("%w: audio/video duration and end drift must be strictly below 0.1s", ErrInvalidExport)
	}
	return info, nil
}

func exportVideoFrame(raw string) (*big.Rat, error) {
	parts := strings.Split(raw, "/")
	if len(parts) == 2 {
		numerator, numErr := strconv.ParseInt(parts[0], 10, 64)
		denominator, denErr := strconv.ParseInt(parts[1], 10, 64)
		if numErr == nil && denErr == nil && numerator > 0 && denominator > 0 {
			return big.NewRat(denominator, numerator), nil
		}
	}
	return nil, fmt.Errorf("%w: avg_frame_rate must be a positive integer rational", ErrInvalidExport)
}

func exportTimestamp(raw string, positive bool) (*big.Rat, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if len(raw) > 128 || strings.ContainsAny(raw, "xXpP_") || err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || (positive && !validDuration(value)) {
		return nil, errors.New("missing or invalid timestamp")
	}
	// Bound big.Rat exponent work, including zero with an enormous exponent.
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(raw[index+1:])
		if err != nil || exponent < -400 || exponent > 400 {
			return nil, errors.New("timestamp exponent out of range")
		}
	}
	rational, ok := new(big.Rat).SetString(raw)
	if !ok || rational.Sign() < 0 {
		return nil, errors.New("invalid decimal timestamp")
	}
	return rational, nil
}

func exportTimeDifference(a, b *big.Rat) *big.Rat {
	return new(big.Rat).Abs(new(big.Rat).Sub(a, b))
}
