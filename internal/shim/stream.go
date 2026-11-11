package shim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/transcript"
	"github.com/coder/websocket"
)

const streamArgument = "--fengshen-stream-v1"

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeWhisperStream) {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(componentprotocol.MaxWSFrame + 1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	start, err := componentprotocol.DecodeStream(data)
	if err != nil || start.Type != "start" {
		writeStreamError(ctx, c, start, "component_request")
		return
	}
	args := []string{streamArgument, "--model", start.Models["whisper-asr"]}
	if start.VAD {
		args = append(args, "--vad", "--vad-model", start.Models["whisper-vad"])
	}
	child, err := s.startChild(ctx, s.cfg.Target, args, streamLimits(*start.DurationSec))
	if err != nil {
		writeStreamError(ctx, c, start, "component_failed")
		return
	}
	defer child.Close()
	go func() { _, _ = io.Copy(io.Discard, child.Stderr()) }()

	if err := writeChildLine(child.Stdin(), childMessage(data)); err != nil {
		writeStreamError(ctx, c, start, "component_failed")
		return
	}
	reader := bufio.NewReaderSize(child.Stdout(), componentprotocol.MaxWSFrame+2)
	line, err := readChildLine(reader)
	if err != nil {
		writeStreamError(ctx, c, start, "component_failed")
		return
	}
	if err := c.Write(ctx, websocket.MessageText, childLine(line)); err != nil {
		return
	}

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		msg, err := componentprotocol.DecodeStream(data)
		if err != nil || msg.RequestID != start.RequestID || msg.EpisodeNumber != start.EpisodeNumber || (msg.Type != "audio" && msg.Type != "finalize") {
			writeStreamError(ctx, c, start, "component_request")
			return
		}
		if msg.Type == "audio" && msg.Revision < 1 || msg.Type == "finalize" && msg.Revision < 1 {
			writeStreamError(ctx, c, start, "component_request")
			return
		}
		if err := writeChildLine(child.Stdin(), childMessage(data)); err != nil {
			writeStreamError(ctx, c, start, "component_failed")
			return
		}
		line, err := readChildLine(reader)
		if err != nil {
			writeStreamError(ctx, c, start, "component_failed")
			return
		}
		if err := c.Write(ctx, websocket.MessageText, childLine(line)); err != nil {
			return
		}
		if msg.Type != "finalize" {
			continue
		}
		_ = child.Stdin().Close()
		waitCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				c.Close(websocket.StatusInternalError, "component failed")
			}
		case <-waitCtx.Done():
			c.Close(websocket.StatusInternalError, "component timeout")
		}
		return
	}
}

func streamLimits(durationSec float64) Limits {
	samples := int64(durationSec * 16000)
	chunks := (samples + 31999) / 32000
	return Limits{In: samples*3 + chunks*1024 + 4096, Out: (chunks + 2) * (1 << 20), Err: 4 << 20, Buffer: 2 << 20}
}

func writeChildLine(w io.Writer, raw []byte) error {
	if len(raw) > componentprotocol.MaxWSFrame {
		return errors.New("shim_frame_too_large")
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n"))
	return err
}

// childStartMessage converts the WS envelope to the legacy NDJSON start message.
// Model bindings stay shim-owned and are not forwarded to the child.
func childMessage(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return raw
	}
	delete(fields, "models")
	delete(fields, "vad")
	fields["protocol"], _ = json.Marshal(transcript.StreamProtocolVersion)
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

func childLine(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte(transcript.StreamProtocolVersion), []byte(componentprotocol.StreamWSv1))
}

func readChildLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	if len(line) > componentprotocol.MaxWSFrame {
		return nil, errors.New("shim_frame_too_large")
	}
	if n := len(line); n >= 2 && line[n-2] == '\r' && line[n-1] == '\n' {
		line = line[:n-2]
	} else if n >= 1 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if len(line) == 0 || !json.Valid(line) {
		return nil, errors.New("shim_frame_invalid")
	}
	return line, nil
}

func writeStreamError(ctx context.Context, c *websocket.Conn, start componentprotocol.StreamMessage, code string) {
	msg, _ := json.Marshal(map[string]any{
		"protocol":       componentprotocol.StreamWSv1,
		"type":           "error",
		"request_id":     start.RequestID,
		"episode_number": start.EpisodeNumber,
		"error_code":     code,
	})
	_ = c.Write(ctx, websocket.MessageText, msg)
	c.Close(websocket.StatusInternalError, "stream failed")
}
