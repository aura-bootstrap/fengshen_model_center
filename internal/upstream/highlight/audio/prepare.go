package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/media"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/workspace"
)

// ProbeClient is implemented by media.FFmpegClient.
type ProbeClient interface {
	Probe(context.Context, string) (media.Info, error)
}

// FFmpegPreparer extracts one fixed-format WAV using the shared media.Runner.
type FFmpegPreparer struct {
	FFmpegPath      string
	FFprobePath     string
	Runner          media.Runner
	Prober          ProbeClient
	MaxWAVBytes     int64
	TimelineOptions TimelineOptions
}

var _ AudioPreparer = (*FFmpegPreparer)(nil)

// NewFFmpegPreparer constructs an injectable preparer. Empty executable paths
// default to ffmpeg/ffprobe and a nil runner defaults to media.ExecRunner.
func NewFFmpegPreparer(ffmpegPath, ffprobePath string, runner media.Runner) *FFmpegPreparer {
	return &FFmpegPreparer{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath, Runner: runner}
}

// AnalysisWAVPath returns the fixed per-episode local output location.
func AnalysisWAVPath(taskTempDir string, episodeNumber int) string {
	return filepath.Join(taskTempDir, fmt.Sprintf("E%03d", episodeNumber), "audio.wav")
}

func (p *FFmpegPreparer) Prepare(ctx context.Context, request AudioPrepareRequest) (prepared PreparedAudio, returnErr error) {
	episodeNumber := request.EpisodeNumber
	normalized, err := request.normalized()
	if err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: episodeNumber, Kind: FailureRequest, Err: err}
	}
	request = normalized
	if err := ctx.Err(); err != nil {
		return PreparedAudio{}, err
	}
	root, err := filepath.Abs(request.TaskTempDir)
	if err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailurePath, Err: errors.Join(ErrUnsafeOutput, err)}
	}
	root = filepath.Clean(root)
	episodeDir := filepath.Dir(AnalysisWAVPath(root, request.EpisodeNumber))
	outputPath := AnalysisWAVPath(root, request.EpisodeNumber)
	if err := workspace.ValidatePath(root, episodeDir); err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailurePath, Err: errors.Join(ErrUnsafeOutput, err)}
	}
	defer func() {
		if returnErr == nil {
			return
		}
		if cleanupErr := removePartialWAV(root, outputPath); cleanupErr != nil {
			returnErr = &PreparationError{
				EpisodeNumber: request.EpisodeNumber,
				Kind:          ClassifyFailure(returnErr),
				Err:           errors.Join(returnErr, cleanupErr),
			}
		}
	}()

	info, err := p.probeClient().Probe(ctx, request.SourceVideoPath)
	if err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureExtraction, Err: errors.Join(ErrExtraction, err)}
	}
	if !info.HasAudio {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureExtraction, Err: ErrNoAudioStream}
	}
	if request.MediaDurationSec > 0 {
		option, optionErr := p.timelineOptions()
		if optionErr != nil {
			return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureRequest, Err: optionErr}
		}
		if !positiveFinite(info.DurationSec) || abs(info.DurationSec-request.MediaDurationSec) > option.MaxDriftSec {
			return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureTimeline, Err: ErrTimeline}
		}
	}

	if err := os.Mkdir(episodeDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailurePath, Err: errors.Join(ErrUnsafeOutput, err)}
	}
	if err := workspace.ValidatePath(root, outputPath); err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailurePath, Err: errors.Join(ErrUnsafeOutput, err)}
	}
	_, _, commandErr := p.commandRunner().Run(ctx,
		p.ffmpegPath(),
		"-hide_banner",
		"-loglevel", "error",
		"-i", request.SourceVideoPath,
		"-map", "0:a:0",
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "pcm_s16le",
		"-f", "wav",
		"-y", outputPath,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		commandErr = errors.Join(commandErr, ctxErr)
	}
	if commandErr != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureExtraction, Err: errors.Join(ErrExtraction, commandErr)}
	}
	if err := workspace.ValidateRegularFile(root, outputPath); err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailurePath, Err: errors.Join(ErrUnsafeOutput, err)}
	}

	maxWAVBytes := int64(0)
	if p != nil {
		maxWAVBytes = p.MaxWAVBytes
	}
	pcm, err := ReadWAV(ctx, outputPath, ReadOptions{MaxBytes: maxWAVBytes, ExpectedFormat: request.Format})
	if err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: ClassifyFailure(err), Err: err}
	}
	prepared = PreparedAudio{
		SourceVideoPath:  request.SourceVideoPath,
		WAVPath:          outputPath,
		EpisodeNumber:    request.EpisodeNumber,
		DurationSec:      pcm.DurationSec,
		MediaDurationSec: info.DurationSec,
		SampleCount:      int64(len(pcm.Samples)),
		Format:           pcm.Format,
		ContentSHA256:    pcm.ContentSHA256,
	}
	mediaDuration := info.DurationSec
	if request.MediaDurationSec > 0 {
		mediaDuration = request.MediaDurationSec
	}
	timelineOptions := TimelineOptions{}
	if p != nil {
		timelineOptions = p.TimelineOptions
	}
	if err := ValidateTimeline(prepared, mediaDuration, domain.Transcript{}, timelineOptions); err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureTimeline, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureExtraction, Err: err}
	}
	return prepared, nil
}

func (p *FFmpegPreparer) commandRunner() media.Runner {
	if p != nil && p.Runner != nil {
		return p.Runner
	}
	return media.ExecRunner{}
}

func (p *FFmpegPreparer) probeClient() ProbeClient {
	if p != nil && p.Prober != nil {
		return p.Prober
	}
	var ffmpegPath, ffprobePath string
	var runner media.Runner
	if p != nil {
		ffmpegPath, ffprobePath, runner = p.FFmpegPath, p.FFprobePath, p.Runner
	}
	return media.NewClient(ffmpegPath, ffprobePath, runner)
}

func (p *FFmpegPreparer) ffmpegPath() string {
	if p != nil && strings.TrimSpace(p.FFmpegPath) != "" {
		return p.FFmpegPath
	}
	return "ffmpeg"
}

func (p *FFmpegPreparer) timelineOptions() (TimelineOptions, error) {
	if p == nil {
		return (TimelineOptions{}).normalized()
	}
	return p.TimelineOptions.normalized()
}

func removePartialWAV(root, path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return workspace.RemoveRegularFile(root, path)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
