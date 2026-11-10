package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sync"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	StreamProtocolVersion        = "fengshen.whispercpp.stream/v1"
	StreamProtocolArgument       = "--fengshen-stream-v1"
	DefaultMaxStreamMessageBytes = 1 << 20
	maxAllowedStreamMessageBytes = 16 << 20
	defaultStreamPCMChunkSamples = 32_000
	maxAllowedStreamChunkSamples = 320_000
	streamBackendErrorMaxLength  = 64
)

var (
	streamRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	streamErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// streamWireMessage is the strict NDJSON envelope used in both directions.
// Audio StartSec/EndSec and every transcript timestamp are absolute source
// times, not offsets relative to an independent chunk.
type streamWireMessage struct {
	Protocol            string                  `json:"protocol"`
	Type                string                  `json:"type"`
	RequestID           string                  `json:"request_id"`
	EpisodeNumber       int                     `json:"episode_number,omitempty"`
	Revision            uint64                  `json:"revision,omitempty"`
	CommittedThroughSec *float64                `json:"committed_through_sec,omitempty"`
	StartSec            *float64                `json:"start_sec,omitempty"`
	EndSec              *float64                `json:"end_sec,omitempty"`
	DurationSec         *float64                `json:"duration_sec,omitempty"`
	SampleCount         int                     `json:"sample_count,omitempty"`
	PCM                 string                  `json:"pcm,omitempty"`
	Format              string                  `json:"format,omitempty"`
	Options             *StreamOptions          `json:"options,omitempty"`
	Items               []domain.TranscriptItem `json:"items,omitempty"`
	Final               bool                    `json:"final,omitempty"`
	ErrorCode           string                  `json:"error_code,omitempty"`
}

type ndjsonStream struct {
	reader *bufio.Reader
	writer io.Writer
	max    int
}

func newNDJSONStream(reader io.Reader, writer io.Writer, max int) (*ndjsonStream, error) {
	if max == 0 {
		max = DefaultMaxStreamMessageBytes
	}
	if max < 4096 || max > maxAllowedStreamMessageBytes {
		return nil, fmt.Errorf("流式 NDJSON 消息上限须在 4096..%d 之间", maxAllowedStreamMessageBytes)
	}
	return &ndjsonStream{reader: bufio.NewReaderSize(reader, max+1), writer: writer, max: max}, nil
}

func (s *ndjsonStream) Send(message streamWireMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return streamProtocolError("message_encode")
	}
	data = append(data, '\n')
	if len(data) > s.max {
		return streamProtocolError("message_too_large")
	}
	if _, err := s.writer.Write(data); err != nil {
		return streamProtocolError("message_write")
	}
	return nil
}

func (s *ndjsonStream) Receive() (streamWireMessage, error) {
	line, err := s.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > s.max {
		return streamWireMessage{}, streamProtocolError("message_too_large")
	}
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		if errors.Is(err, io.EOF) {
			return streamWireMessage{}, io.EOF
		}
		return streamWireMessage{}, streamProtocolError("message_read")
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return streamWireMessage{}, streamProtocolError("empty_message")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var message streamWireMessage
	if err := decoder.Decode(&message); err != nil {
		return streamWireMessage{}, streamProtocolError("message_decode")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return streamWireMessage{}, streamProtocolError("message_trailing_json")
	}
	if message.Protocol != StreamProtocolVersion || !streamRequestIDPattern.MatchString(message.RequestID) {
		return streamWireMessage{}, streamProtocolError("message_identity")
	}
	return message, nil
}

type streamProcess interface {
	Send(streamWireMessage) error
	Receive() (streamWireMessage, error)
	Close() error
}

type streamProcessFactory func(context.Context, ...string) (streamProcess, error)

type execStreamProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	codec    *ndjsonStream
	close    sync.Once
	closeErr error
}

// startExecStreamProcess executes an explicit argv directly. No shell, batch
// whisper-cli -f argument, intermediate chunk WAV, or stderr logging is used.
func startExecStreamProcess(ctx context.Context, args ...string) (streamProcess, error) {
	if len(args) == 0 || args[0] == "" {
		return nil, ErrStreamingBackendUnavailable
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, ErrStreamingBackendUnavailable
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, ErrStreamingBackendUnavailable
	}
	// Native diagnostics are deliberately not persisted: they may contain model
	// output or transcript text. Discarding also prevents a stderr pipe deadlock.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, ErrStreamingBackendUnavailable
	}
	codec, err := newNDJSONStream(stdout, stdin, DefaultMaxStreamMessageBytes)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	return &execStreamProcess{cmd: cmd, stdin: stdin, codec: codec}, nil
}

func (p *execStreamProcess) Send(message streamWireMessage) error {
	return p.codec.Send(message)
}

func (p *execStreamProcess) Receive() (streamWireMessage, error) {
	return p.codec.Receive()
}

func (p *execStreamProcess) Close() error {
	p.close.Do(func() {
		p.closeErr = p.stdin.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		if err := p.cmd.Wait(); p.closeErr == nil {
			p.closeErr = err
		}
	})
	return p.closeErr
}
