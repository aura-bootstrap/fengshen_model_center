package transcript

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

// StreamingWhisperCPP drives an explicitly compatible native whisper.cpp
// streaming executable. One process and one decoder request remain alive while
// bounded PCM messages advance through the prepared episode.
type StreamingWhisperCPP struct {
	CLIPath         string
	ModelPath       string
	Threads         int
	VAD             bool
	VADModelPath    string
	MaxWAVBytes     int64
	MaxMessageBytes int
	PCMChunkSamples int
	Diagnostics     func(domain.Diagnostic)

	processFactory streamProcessFactory
}

func (w *StreamingWhisperCPP) Identity() string {
	model := w.ModelPath
	if absolute, err := filepath.Abs(model); err == nil {
		model = filepath.Clean(absolute)
	}
	identity := "whisper-cli:stream-v1:" + w.CLIPath + ";model=" + model
	if info, err := os.Stat(w.ModelPath); err == nil {
		identity += ";size=" + strconv.FormatInt(info.Size(), 10) + ";mtime=" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	}
	return identity
}

func (w *StreamingWhisperCPP) TranscribeStream(ctx context.Context, episode domain.Episode, prepared audio.PreparedAudio, options StreamOptions, emit func(StreamCommit) error) error {
	options = options.WithDefaults()
	if err := options.Validate(); err != nil {
		return err
	}
	if emit == nil {
		return fmt.Errorf("流式稳定提交回调不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(w.CLIPath) == "" || strings.TrimSpace(w.ModelPath) == "" {
		return fmt.Errorf("%w: 缺少 whisper_stream_cli_path 或模型", ErrStreamingBackendUnavailable)
	}
	if err := prepared.Validate(); err != nil {
		return fmt.Errorf("预备分析音轨无效: %w", err)
	}
	if prepared.EpisodeNumber != episode.EpisodeNumber || strings.TrimSpace(prepared.SourceVideoPath) == "" ||
		!sameSourcePath(prepared.SourceVideoPath, episode.VideoPath) {
		return fmt.Errorf("预备分析音轨与源视频身份不匹配")
	}
	info, err := os.Lstat(prepared.WAVPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("预备分析音轨不是可读取的普通文件")
	}
	pcm, err := audio.ReadWAV(ctx, prepared.WAVPath, audio.ReadOptions{MaxBytes: w.MaxWAVBytes, ExpectedFormat: prepared.Format})
	if err != nil {
		return err
	}
	if int64(len(pcm.Samples)) != prepared.SampleCount || math.Abs(pcm.DurationSec-prepared.DurationSec) > 1/float64(prepared.Format.SampleRate) ||
		pcm.ContentSHA256 != prepared.ContentSHA256 {
		return fmt.Errorf("预备分析音轨内容身份不匹配: %w", audio.ErrTimeline)
	}
	maxMessageBytes, chunkSamples, err := w.protocolLimits()
	if err != nil {
		return err
	}
	committer, err := NewStablePrefixCommitter(episode, prepared, w.Identity(), options, w.Diagnostics)
	if err != nil {
		return err
	}
	factory := w.processFactory
	if factory == nil {
		factory = startExecStreamProcess
	}
	process, err := factory(ctx, w.streamArguments()...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: 原生协议进程启动失败", ErrStreamingBackendUnavailable)
	}
	defer process.Close()

	requestID := fmt.Sprintf("E%03d-%s", episode.EpisodeNumber, prepared.ContentSHA256[:16])
	resolvedOptions := options
	duration := prepared.DurationSec
	start := streamWireMessage{
		Protocol: StreamProtocolVersion, Type: "start", RequestID: requestID,
		EpisodeNumber: episode.EpisodeNumber, DurationSec: &duration,
		Format: "s16le/16000/1", Options: &resolvedOptions,
	}
	if err := sendBoundedStreamMessage(process, start, maxMessageBytes); err != nil {
		return streamProcessError(ctx, err)
	}
	if _, err := receiveStreamMessage(ctx, process, "ready", requestID, 0); err != nil {
		return err
	}

	for offset, revision := 0, uint64(1); offset < len(pcm.Samples); revision++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + chunkSamples
		if end > len(pcm.Samples) {
			end = len(pcm.Samples)
		}
		startSec := float64(offset) / float64(pcm.Format.SampleRate)
		endSec := float64(end) / float64(pcm.Format.SampleRate)
		message := streamWireMessage{
			Protocol: StreamProtocolVersion, Type: "audio", RequestID: requestID,
			EpisodeNumber: episode.EpisodeNumber, Revision: revision,
			StartSec: &startSec, EndSec: &endSec, SampleCount: end - offset,
			PCM: base64.StdEncoding.EncodeToString(pcmBytes(pcm.Samples[offset:end])),
		}
		if err := sendBoundedStreamMessage(process, message, maxMessageBytes); err != nil {
			return streamProcessError(ctx, err)
		}
		response, err := receiveStreamMessage(ctx, process, "hypothesis", requestID, revision)
		if err != nil {
			return err
		}
		if response.CommittedThroughSec == nil || math.Abs(*response.CommittedThroughSec-endSec) > 1e-9 || response.Final {
			return streamProtocolError("audio_response_boundary")
		}
		commit, err := committer.Push(StreamHypothesis{
			EpisodeNumber: episode.EpisodeNumber, Revision: revision,
			CommittedThroughSec: endSec, Items: response.Items,
		})
		if err != nil {
			return err
		}
		if commit != nil {
			if err := emit(cloneStreamCommit(*commit)); err != nil {
				return err
			}
		}
		offset = end
	}

	finalRevision := uint64((len(pcm.Samples)+chunkSamples-1)/chunkSamples) + 1
	finalize := streamWireMessage{
		Protocol: StreamProtocolVersion, Type: "finalize", RequestID: requestID,
		EpisodeNumber: episode.EpisodeNumber, Revision: finalRevision, CommittedThroughSec: &duration,
	}
	if err := sendBoundedStreamMessage(process, finalize, maxMessageBytes); err != nil {
		return streamProcessError(ctx, err)
	}
	response, err := receiveStreamMessage(ctx, process, "final", requestID, finalRevision)
	if err != nil {
		return err
	}
	if !response.Final || response.CommittedThroughSec == nil || math.Abs(*response.CommittedThroughSec-duration) > 1e-9 {
		return streamProtocolError("final_response_boundary")
	}
	commit, err := committer.Push(StreamHypothesis{
		EpisodeNumber: episode.EpisodeNumber, Revision: finalRevision,
		CommittedThroughSec: duration, Items: response.Items, Final: true,
	})
	if err != nil {
		return err
	}
	if commit == nil || !commit.Final {
		return streamProtocolError("final_commit_missing")
	}
	return emit(cloneStreamCommit(*commit))
}

func (w *StreamingWhisperCPP) streamArguments() []string {
	threads := w.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	args := []string{w.CLIPath, StreamProtocolArgument, "--model", w.ModelPath, "--threads", strconv.Itoa(threads)}
	if w.VAD {
		args = append(args, "--vad")
		if w.VADModelPath != "" {
			args = append(args, "--vad-model", w.VADModelPath)
		}
	}
	return args
}

func (w *StreamingWhisperCPP) protocolLimits() (int, int, error) {
	maxMessageBytes := w.MaxMessageBytes
	if maxMessageBytes == 0 {
		maxMessageBytes = DefaultMaxStreamMessageBytes
	}
	if maxMessageBytes < 4096 || maxMessageBytes > maxAllowedStreamMessageBytes {
		return 0, 0, fmt.Errorf("流式 NDJSON 消息上限无效")
	}
	chunkSamples := w.PCMChunkSamples
	if chunkSamples == 0 {
		chunkSamples = defaultStreamPCMChunkSamples
	}
	if chunkSamples < 1 || chunkSamples > maxAllowedStreamChunkSamples {
		return 0, 0, fmt.Errorf("流式 PCM 块大小无效")
	}
	return maxMessageBytes, chunkSamples, nil
}

func sendBoundedStreamMessage(process streamProcess, message streamWireMessage, max int) error {
	if bounded, ok := process.(*execStreamProcess); ok && bounded.codec.max != max {
		codec, err := newNDJSONStream(bounded.codec.reader, bounded.codec.writer, max)
		if err != nil {
			return err
		}
		bounded.codec = codec
	}
	return process.Send(message)
}

func receiveStreamMessage(ctx context.Context, process streamProcess, expectedType, requestID string, revision uint64) (streamWireMessage, error) {
	message, err := process.Receive()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return streamWireMessage{}, ctxErr
		}
		if errors.Is(err, io.EOF) {
			return streamWireMessage{}, streamProtocolError("unexpected_eof")
		}
		return streamWireMessage{}, err
	}
	if message.Type == "error" {
		code := "backend_error"
		if streamErrorCodePattern.MatchString(message.ErrorCode) && len(message.ErrorCode) <= streamBackendErrorMaxLength {
			code = "backend_" + message.ErrorCode
		}
		return streamWireMessage{}, streamProtocolError(code)
	}
	if message.Type != expectedType || message.RequestID != requestID {
		return streamWireMessage{}, streamProtocolError("response_identity")
	}
	if expectedType != "ready" && message.Revision != revision {
		return streamWireMessage{}, streamProtocolError("response_revision")
	}
	return message, nil
}

func streamProcessError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func pcmBytes(samples []int16) []byte {
	data := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(data[index*2:index*2+2], uint16(sample))
	}
	return data
}
