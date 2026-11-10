package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

var (
	// ErrInvalidDuration identifies a missing, non-finite, or non-positive
	// media duration.
	ErrInvalidDuration = errors.New("invalid media duration")
	// ErrInvalidRequest identifies invalid paths or time ranges before an
	// external command is started.
	ErrInvalidRequest = errors.New("invalid media request")
)

// Client is the media boundary consumed by the analysis pipeline.
type Client interface {
	Duration(context.Context, string) (float64, error)
	SceneBoundaries(context.Context, string, float64) ([]float64, error)
	ContactSheet(context.Context, SheetRequest) error
	ExportClip(context.Context, ExportRequest) error
}

// SheetRequest describes one candidate contact sheet. OutputPath must be
// unique to the candidate when sheets are generated concurrently.
type SheetRequest struct {
	SourcePath string
	OutputPath string
	StartSec   float64
	EndSec     float64
}

// ExportRequest describes one independently encoded highlight clip.
type ExportRequest struct {
	SourceCut     *domain.SourceCutEvidence
	SourceVersion string
	SourcePath    string
	OutputPath    string
	StartSec      float64
	EndSec        float64
}

// FFmpegClient implements Client using ffprobe and FFmpeg. Empty executable
// paths default to "ffmpeg" and "ffprobe"; a nil Runner uses ExecRunner.
type FFmpegClient struct {
	FFmpegPath     string
	FFprobePath    string
	Runner         Runner
	SceneThreshold float64
}

// NewClient constructs an injectable FFmpeg media client.
func NewClient(ffmpegPath, ffprobePath string, runner Runner) *FFmpegClient {
	return &FFmpegClient{
		FFmpegPath:  ffmpegPath,
		FFprobePath: ffprobePath,
		Runner:      runner,
	}
}

var _ Client = (*FFmpegClient)(nil)

// Duration probes the container duration and rejects invalid values.
func (c *FFmpegClient) Duration(ctx context.Context, videoPath string) (float64, error) {
	if strings.TrimSpace(videoPath) == "" {
		return 0, fmt.Errorf("%w: video path is required", ErrInvalidRequest)
	}
	stdout, _, err := c.commandRunner().Run(ctx,
		c.ffprobePath(),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoPath,
	)
	if err != nil {
		return 0, fmt.Errorf("probe media duration: %w", err)
	}

	text := strings.TrimSpace(stdout)
	duration, parseErr := strconv.ParseFloat(text, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidDuration, parseErr)
	}
	if !validDuration(duration) {
		return 0, fmt.Errorf("%w: duration must be finite and greater than zero", ErrInvalidDuration)
	}
	return duration, nil
}

var ptsTimePattern = regexp.MustCompile(`pts_time:([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)`)

// SceneBoundaries detects scene changes with select/showinfo and returns
// sorted, unique timestamps including the video start and end.
func (c *FFmpegClient) SceneBoundaries(ctx context.Context, videoPath string, duration float64) ([]float64, error) {
	if strings.TrimSpace(videoPath) == "" {
		return nil, fmt.Errorf("%w: video path is required", ErrInvalidRequest)
	}
	if !validDuration(duration) {
		return nil, fmt.Errorf("%w: duration must be finite and greater than zero", ErrInvalidDuration)
	}

	threshold := c.SceneThreshold
	if threshold <= 0 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		threshold = 0.3
	}
	filter := "select='gt(scene," + strconv.FormatFloat(threshold, 'f', -1, 64) + ")',showinfo"
	_, stderr, err := c.commandRunner().Run(ctx,
		c.ffmpegPath(),
		"-hide_banner",
		"-i", videoPath,
		"-vf", filter,
		"-an",
		"-f", "null",
		"-",
	)
	if err != nil {
		return nil, fmt.Errorf("detect scene boundaries: %w", err)
	}

	unique := map[float64]struct{}{0: {}, duration: {}}
	for _, match := range ptsTimePattern.FindAllStringSubmatch(stderr, -1) {
		if len(match) != 2 {
			continue
		}
		value, parseErr := strconv.ParseFloat(match[1], 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if value > 0 && value < duration {
			unique[value] = struct{}{}
		}
	}

	boundaries := make([]float64, 0, len(unique))
	for boundary := range unique {
		boundaries = append(boundaries, boundary)
	}
	sort.Float64s(boundaries)
	return boundaries, nil
}

// ContactSheet samples at most six frames, scales them to 320 pixels wide,
// and writes one 3x2 JPEG sheet.
func (c *FFmpegClient) ContactSheet(ctx context.Context, request SheetRequest) error {
	if err := validateMediaRequest(request.SourcePath, request.OutputPath, request.StartSec, request.EndSec); err != nil {
		return err
	}

	duration := math.Max(0.01, request.EndSec-request.StartSec)
	interval := math.Max(1, duration/6)
	filter := "fps=1/" + formatSeconds(interval) + ",scale=320:-1,tile=3x2"
	_, _, err := c.commandRunner().Run(ctx,
		c.ffmpegPath(),
		"-hide_banner",
		"-ss", formatSeconds(request.StartSec),
		"-i", request.SourcePath,
		"-t", formatSeconds(duration),
		"-vf", filter,
		"-frames:v", "1",
		"-q:v", "3",
		"-y", request.OutputPath,
	)
	if err != nil {
		return fmt.Errorf("create contact sheet: %w", err)
	}
	return nil
}

// ExportClip re-encodes one range as H.264/AAC MP4 and removes its partial
// output when FFmpeg fails or the context is canceled.
func (c *FFmpegClient) ExportClip(ctx context.Context, request ExportRequest) error {
	if err := validateMediaRequest(request.SourcePath, request.OutputPath, request.StartSec, request.EndSec); err != nil {
		return err
	}

	if request.SourceCut != nil {
		return c.exportSourceCut(ctx, request)
	}
	_, _, err := c.commandRunner().Run(ctx,
		c.ffmpegPath(),
		"-ss", formatSeconds(request.StartSec),
		"-i", request.SourcePath,
		"-t", formatSeconds(request.EndSec-request.StartSec),
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "setpts=PTS-STARTPTS",
		"-af", "asetpts=PTS-STARTPTS",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-c:a", "aac",
		"-b:a", "160k",
		"-y", request.OutputPath,
	)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		return nil
	}

	exportErr := fmt.Errorf("export clip: %w", err)
	if removeErr := os.Remove(request.OutputPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return errors.Join(exportErr, fmt.Errorf("remove partial clip: %w", removeErr))
	}
	return exportErr
}

// ExportSource is the design-facing name for ExportClip.
func (c *FFmpegClient) ExportSource(ctx context.Context, request ExportRequest) error {
	return c.ExportClip(ctx, request)
}

// ContactSheetPath returns a collision-free path for concurrent candidates.
func ContactSheetPath(taskDir string, episodeNumber, candidateIndex int) string {
	return filepath.Join(taskDir, fmt.Sprintf("E%03d", episodeNumber), fmt.Sprintf("candidate_%03d.jpg", candidateIndex+1))
}

func (c *FFmpegClient) commandRunner() Runner {
	if c != nil && c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c *FFmpegClient) ffmpegPath() string {
	if c != nil && c.FFmpegPath != "" {
		return c.FFmpegPath
	}
	return "ffmpeg"
}

func (c *FFmpegClient) ffprobePath() string {
	if c != nil && c.FFprobePath != "" {
		return c.FFprobePath
	}
	return "ffprobe"
}

func validateMediaRequest(sourcePath, outputPath string, start, end float64) error {
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(outputPath) == "" {
		return fmt.Errorf("%w: source and output paths are required", ErrInvalidRequest)
	}
	if math.IsNaN(start) || math.IsInf(start, 0) || math.IsNaN(end) || math.IsInf(end, 0) || start < 0 || end <= start {
		return fmt.Errorf("%w: time range must be finite and satisfy 0 <= start < end", ErrInvalidRequest)
	}
	return nil
}

func validDuration(duration float64) bool {
	return duration > 0 && !math.IsNaN(duration) && !math.IsInf(duration, 0)
}

func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}
