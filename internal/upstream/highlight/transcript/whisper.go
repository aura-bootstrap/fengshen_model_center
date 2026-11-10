package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/audio"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

type Runner interface {
	Run(context.Context, ...string) (stdout, stderr string, err error)
}

type WhisperCPP struct {
	CLIPath      string
	ModelPath    string
	FFmpegPath   string
	WorkDir      string
	Threads      int
	VAD          bool
	VADModelPath string
	Runner       Runner
}

func (w *WhisperCPP) Identity() string {
	model := w.ModelPath
	if absolute, err := filepath.Abs(model); err == nil {
		model = filepath.Clean(absolute)
	}
	identity := "whisper-cli:" + w.CLIPath + ";model=" + model
	if info, err := os.Stat(w.ModelPath); err == nil {
		identity += ";size=" + strconv.FormatInt(info.Size(), 10) + ";mtime=" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	}
	return identity
}

func (w *WhisperCPP) Transcribe(ctx context.Context, episode domain.Episode, progress ProgressFunc) (domain.Transcript, error) {
	return w.transcribe(ctx, episode, nil, progress)
}

// TranscribeWithAudio runs whisper-cli directly on the already validated
// analysis WAV. No ffmpeg command is invoked on this path.
func (w *WhisperCPP) TranscribeWithAudio(ctx context.Context, episode domain.Episode, prepared audio.PreparedAudio, progress ProgressFunc) (domain.Transcript, error) {
	return w.transcribe(ctx, episode, &prepared, progress)
}

func (w *WhisperCPP) transcribe(ctx context.Context, episode domain.Episode, prepared *audio.PreparedAudio, progress ProgressFunc) (domain.Transcript, error) {
	if w.Runner == nil || w.CLIPath == "" || w.ModelPath == "" || w.WorkDir == "" || (prepared == nil && w.FFmpegPath == "") {
		return domain.Transcript{}, fmt.Errorf("whisper-cli 配置不完整")
	}
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	if err := os.MkdirAll(w.WorkDir, 0o700); err != nil {
		return domain.Transcript{}, err
	}
	episodeWorkDir := filepath.Join(w.WorkDir, fmt.Sprintf("E%03d", episode.EpisodeNumber))
	if err := os.MkdirAll(episodeWorkDir, 0o700); err != nil {
		return domain.Transcript{}, err
	}
	audioPath := ""
	if prepared != nil {
		if err := prepared.Validate(); err != nil {
			return domain.Transcript{}, fmt.Errorf("预备分析音轨无效: %w", err)
		}
		if prepared.SourceVideoPath != "" && !sameSourcePath(prepared.SourceVideoPath, episode.VideoPath) {
			return domain.Transcript{}, fmt.Errorf("预备分析音轨与源视频身份不匹配")
		}
		if prepared.EpisodeNumber != episode.EpisodeNumber {
			return domain.Transcript{}, fmt.Errorf("预备分析音轨集数不匹配")
		}
		info, err := os.Lstat(prepared.WAVPath)
		if err != nil || !info.Mode().IsRegular() {
			return domain.Transcript{}, fmt.Errorf("预备分析音轨不是可读取的普通文件")
		}
		audioPath = prepared.WAVPath
		report(progress, "复用分析音轨", 5)
	} else {
		report(progress, "抽取转写音频", 5)
		audioPath = filepath.Join(episodeWorkDir, "audio.wav")
		_, _, err := w.Runner.Run(ctx,
			w.FFmpegPath, "-hide_banner", "-loglevel", "error", "-i", episode.VideoPath,
			"-vn", "-ac", "1", "-ar", "16000", "-y", audioPath,
		)
		if err != nil {
			return domain.Transcript{}, fmt.Errorf("FFmpeg 音频抽取失败: %w", err)
		}
	}
	report(progress, "本地 Whisper 转写", 20)
	outputBase := filepath.Join(episodeWorkDir, "transcript")
	threads := w.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	args := []string{
		w.CLIPath, "-m", w.ModelPath, "-f", audioPath, "-l", "zh",
		"-t", strconv.Itoa(threads), "-ml", "1", "-sow", "-ojf", "-of", outputBase, "-np",
	}
	if w.VAD {
		args = append(args, "--vad")
		if w.VADModelPath != "" {
			args = append(args, "--vad-model", w.VADModelPath)
		}
	}
	if _, _, err := w.Runner.Run(ctx, args...); err != nil {
		return domain.Transcript{}, fmt.Errorf("whisper-cli 转写失败: %w", err)
	}
	report(progress, "校验词级时间戳", 90)
	data, err := os.ReadFile(outputBase + ".json")
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("读取 whisper-cli JSON 失败: %w", err)
	}
	items, err := ParseWhisperJSON(data)
	if err != nil {
		return domain.Transcript{}, err
	}
	if prepared != nil {
		duration := prepared.DurationSec
		if prepared.MediaDurationSec > 0 && prepared.MediaDurationSec < duration {
			duration = prepared.MediaDurationSec
		}
		items = clipWhisperItemsToDuration(items, duration)
	}
	report(progress, "转写完成", 100)
	return domain.Transcript{Items: items, Source: w.Identity(), ConfirmedNoSpeech: false}, nil
}

type whisperDocument struct {
	Transcription []whisperSegment `json:"transcription"`
}

type whisperSegment struct {
	Offsets whisperOffsets `json:"offsets"`
	Text    string         `json:"text"`
	Tokens  []whisperToken `json:"tokens"`
}

type whisperOffsets struct {
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}

type whisperToken struct {
	Offsets *whisperOffsets `json:"offsets"`
	Text    string          `json:"text"`
	P       *float64        `json:"p"`
}

func ParseWhisperJSON(data []byte) ([]domain.TranscriptItem, error) {
	var document whisperDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("whisper-cli JSON 无效")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("whisper-cli JSON 含尾随内容")
	}
	items := make([]domain.TranscriptItem, 0, len(document.Transcription))
	for _, segment := range document.Transcription {
		text := strings.TrimSpace(segment.Text)
		if text == "" {
			continue
		}
		if segment.Offsets.From == nil || segment.Offsets.To == nil {
			return nil, fmt.Errorf("whisper-cli segment 缺少时间")
		}
		start, end := float64(*segment.Offsets.From)/1000, float64(*segment.Offsets.To)/1000
		if start == end {
			continue
		}
		if !domain.ValidRange(start, end) {
			return nil, fmt.Errorf("whisper-cli segment 时间无效")
		}
		words, reliable := normalizeWhisperWords(segment)
		item := domain.TranscriptItem{StartSec: start, EndSec: end, Text: text, Words: words}
		if reliable {
			item.Words = []domain.TranscriptWord{{
				StartSec: words[0].StartSec, EndSec: words[len(words)-1].EndSec,
				Text: text, Probability: minimumProbability(words),
			}}
		} else {
			// A non-matching valid atom keeps the ASR statement ineligible without rejecting the episode.
			item.Words = []domain.TranscriptWord{{StartSec: start, EndSec: end}}
		}
		items = append(items, item)
	}
	return items, nil
}

func normalizeWhisperWords(segment whisperSegment) ([]domain.TranscriptWord, bool) {
	words := make([]domain.TranscriptWord, 0, len(segment.Tokens))
	var joined strings.Builder
	reliable := true
	previousStart := int64(-1)
	for _, token := range segment.Tokens {
		if isSpecialToken(token.Text) || strings.TrimSpace(token.Text) == "" {
			continue
		}
		word := domain.TranscriptWord{StartSec: -1, EndSec: -1, Text: token.Text, Probability: token.P}
		if token.Offsets != nil && token.Offsets.From != nil && token.Offsets.To != nil {
			from, to := *token.Offsets.From, *token.Offsets.To
			word.StartSec = float64(from) / 1000
			word.EndSec = float64(to) / 1000
			if from < *segment.Offsets.From || to > *segment.Offsets.To ||
				from >= to || from < previousStart {
				reliable = false
			}
			previousStart = from
		} else {
			reliable = false
		}
		if token.P != nil && !domain.Finite(*token.P) {
			reliable = false
		}
		words = append(words, word)
		joined.WriteString(token.Text)
	}
	if len(words) == 0 || normalizeWhisperText(joined.String()) != normalizeWhisperText(segment.Text) {
		reliable = false
	}
	return words, reliable
}

func clipWhisperItemsToDuration(items []domain.TranscriptItem, duration float64) []domain.TranscriptItem {
	clipped := make([]domain.TranscriptItem, 0, len(items))
	for _, item := range items {
		if item.StartSec >= duration {
			continue
		}
		if item.EndSec > duration {
			item.EndSec = duration
		}
		words := make([]domain.TranscriptWord, 0, len(item.Words))
		for _, word := range item.Words {
			if word.StartSec >= duration {
				continue
			}
			if word.EndSec > duration {
				word.EndSec = duration
			}
			words = append(words, word)
		}
		item.Words = words
		clipped = append(clipped, item)
	}
	return clipped
}

func minimumProbability(words []domain.TranscriptWord) *float64 {
	var minimum *float64
	for _, word := range words {
		if word.Probability == nil {
			continue
		}
		if minimum == nil || *word.Probability < *minimum {
			value := *word.Probability
			minimum = &value
		}
	}
	return minimum
}

func isSpecialToken(text string) bool {
	trimmed := strings.TrimSpace(text)
	return (strings.HasPrefix(trimmed, "<|") && strings.HasSuffix(trimmed, "|>")) ||
		(strings.HasPrefix(trimmed, "[_") && strings.HasSuffix(trimmed, "]"))
}

func normalizeWhisperText(text string) string {
	return strings.Join(strings.Fields(text), "")
}

func report(progress ProgressFunc, message string, percent int) {
	if progress != nil {
		progress(message, percent)
	}
}
