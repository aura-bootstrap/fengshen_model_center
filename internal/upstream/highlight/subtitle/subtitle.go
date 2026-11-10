package subtitle

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

var (
	// ErrEmpty identifies subtitles that contain no usable cues.
	ErrEmpty = errors.New("字幕为空")
	// ErrInvalid identifies malformed subtitle content. No partial transcript is
	// returned when this error occurs.
	ErrInvalid = errors.New("字幕格式无效")
)

var (
	timingLinePattern = regexp.MustCompile(`^(\S+)\s*-->\s*(\S+)(?:\s+.*)?$`)
	timestampPattern  = regexp.MustCompile(`^(?:([0-9]{2}):)?([0-9]{2}):([0-9]{2})[.,]([0-9]{3})$`)
	srtIndexPattern   = regexp.MustCompile(`^[0-9]+$`)
)

// Parse reads and parses an SRT or WebVTT subtitle file.
func Parse(path string, source string) (domain.Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("读取字幕 %q: %w", path, err)
	}
	transcript, err := ParseText(string(data), source)
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("解析字幕 %q: %w", path, err)
	}
	return transcript, nil
}

// ParseText parses SRT or WebVTT text. Multi-line cue text is joined with a
// newline. Sidecar subtitles do not provide word-level timestamps, so Words is
// left empty for every item.
func ParseText(text string, source string) (domain.Transcript, error) {
	if !utf8.ValidString(text) {
		return domain.Transcript{}, fmt.Errorf("%w: 不是有效 UTF-8", ErrInvalid)
	}
	text = strings.TrimPrefix(text, "\uFEFF")
	if strings.TrimSpace(text) == "" {
		return domain.Transcript{}, ErrEmpty
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	index := 0
	skipBlankLines(lines, &index)
	isVTT := false
	if index < len(lines) && isVTTHeader(lines[index]) {
		isVTT = true
		index++
		// WebVTT metadata ends at the first blank line. Also tolerate a cue
		// immediately following the header.
		for index < len(lines) && strings.TrimSpace(lines[index]) != "" && !strings.Contains(lines[index], "-->") {
			index++
		}
	}

	items := make([]domain.TranscriptItem, 0)
	for index < len(lines) {
		skipBlankLines(lines, &index)
		if index >= len(lines) {
			break
		}

		line := strings.TrimSpace(lines[index])
		if isVTT && isSkippedVTTBlock(line) {
			skipVTTBlock(lines, &index)
			continue
		}

		timingIndex := index
		if !strings.Contains(line, "-->") {
			if !isVTT && !validSRTIndex(line) {
				return domain.Transcript{}, fmt.Errorf("%w: 第 %d 行不是有效 SRT 序号", ErrInvalid, index+1)
			}
			timingIndex++
			if timingIndex >= len(lines) || strings.TrimSpace(lines[timingIndex]) == "" {
				return domain.Transcript{}, fmt.Errorf("%w: 第 %d 行后缺少时间轴", ErrInvalid, index+1)
			}
		}

		start, end, err := parseTimingLine(lines[timingIndex])
		if err != nil {
			return domain.Transcript{}, fmt.Errorf("%w: 第 %d 行: %v", ErrInvalid, timingIndex+1, err)
		}
		index = timingIndex + 1
		textLines := make([]string, 0)
		for index < len(lines) && strings.TrimSpace(lines[index]) != "" {
			textLines = append(textLines, strings.TrimSpace(lines[index]))
			index++
		}
		cueText := strings.TrimSpace(strings.Join(textLines, "\n"))
		if cueText == "" {
			return domain.Transcript{}, fmt.Errorf("%w: 第 %d 行时间轴缺少文本", ErrInvalid, timingIndex+1)
		}
		items = append(items, domain.TranscriptItem{StartSec: start, EndSec: end, Text: cueText})
	}

	if len(items) == 0 {
		return domain.Transcript{}, ErrEmpty
	}
	transcript := domain.Transcript{Items: items, Source: source}
	if err := transcript.Validate(); err != nil {
		return domain.Transcript{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return transcript, nil
}

func skipBlankLines(lines []string, index *int) {
	for *index < len(lines) && strings.TrimSpace(lines[*index]) == "" {
		*index++
	}
}

func isVTTHeader(line string) bool {
	line = strings.TrimSpace(line)
	return line == "WEBVTT" || strings.HasPrefix(line, "WEBVTT ") || strings.HasPrefix(line, "WEBVTT\t")
}

func isSkippedVTTBlock(line string) bool {
	return line == "NOTE" || strings.HasPrefix(line, "NOTE ") || strings.HasPrefix(line, "NOTE\t") ||
		line == "STYLE" || line == "REGION"
}

func skipVTTBlock(lines []string, index *int) {
	for *index < len(lines) && strings.TrimSpace(lines[*index]) != "" {
		*index++
	}
}

func validSRTIndex(line string) bool {
	if !srtIndexPattern.MatchString(line) {
		return false
	}
	index, err := strconv.Atoi(line)
	return err == nil && index > 0
}

func parseTimingLine(line string) (float64, float64, error) {
	match := timingLinePattern.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return 0, 0, fmt.Errorf("时间轴格式错误")
	}
	start, err := parseTimestamp(match[1])
	if err != nil {
		return 0, 0, fmt.Errorf("开始时间: %w", err)
	}
	end, err := parseTimestamp(match[2])
	if err != nil {
		return 0, 0, fmt.Errorf("结束时间: %w", err)
	}
	if start >= end {
		return 0, 0, fmt.Errorf("开始时间必须早于结束时间")
	}
	return start, end, nil
}

func parseTimestamp(value string) (float64, error) {
	match := timestampPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("时间戳 %q 格式错误", value)
	}
	hours := 0
	var err error
	if match[1] != "" {
		hours, err = strconv.Atoi(match[1])
		if err != nil {
			return 0, fmt.Errorf("小时无效")
		}
	}
	minutes, err := strconv.Atoi(match[2])
	if err != nil || minutes >= 60 {
		return 0, fmt.Errorf("分钟无效")
	}
	seconds, err := strconv.Atoi(match[3])
	if err != nil || seconds >= 60 {
		return 0, fmt.Errorf("秒无效")
	}
	milliseconds, err := strconv.Atoi(match[4])
	if err != nil {
		return 0, fmt.Errorf("毫秒无效")
	}
	totalMilliseconds := ((hours*60+minutes)*60+seconds)*1000 + milliseconds
	return float64(totalMilliseconds) / 1000, nil
}
