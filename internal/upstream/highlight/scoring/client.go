package scoring

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

const (
	DefaultMinHighlightScore    = 0.70
	DefaultMinCompletenessScore = 0.80
	MaxRequestBytes             = 2 << 20
	MaxResponseBytes            = 1 << 20
	maxContactSheetBytes        = 1 << 20
)

// Client performs bounded OpenAI-compatible Chat Completions requests.
type Client struct {
	config            Config
	httpClient        *http.Client
	constrainedVision bool
}

func NewClient(config Config) (*Client, error) {
	return newClient(config, nil, nil)
}

func newClient(config Config, resolver loopbackResolver, dial dialContextFunc) (*Client, error) {
	config = withConfigDefaults(config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	timeout := time.Duration(config.TimeoutSec) * time.Second
	httpClient := newHTTPClient(timeout)
	if config.Provider == ProviderLocal {
		httpClient = newLoopbackHTTPClient(timeout, resolver, dial)
	}
	return &Client{config: config, httpClient: httpClient}, nil
}

// NewClientWithHTTPClient supports contract tests while preserving redirect,
// timeout and retry behavior configured by the caller.
func NewClientWithHTTPClient(config Config, httpClient *http.Client) (*Client, error) {
	client, err := NewClient(config)
	if err != nil {
		return nil, err
	}
	if httpClient != nil {
		if client.config.Provider == ProviderLocal {
			return nil, newError(ErrorKindLocalModelConfig, 0, client.config.APIKey, "local provider requires its dedicated loopback transport", nil)
		}
		client.httpClient = httpClient
		client.httpClient.Timeout = time.Duration(client.config.TimeoutSec) * time.Second
		client.httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client, nil
}

func (c *Client) ScoreEvents(ctx context.Context, request CandidateInput) ([]*RawEvent, error) {
	result, err := c.Score(ctx, request)
	if err != nil {
		return nil, err
	}
	return result.Events, nil
}

// Score executes one logical request with bounded same-provider retries.
func (c *Client) Score(ctx context.Context, request CandidateInput) (Result, error) {
	request, err := normalizeScoringRequest(request)
	if err != nil {
		return Result{}, err
	}
	body, imageBytes, err := c.buildRequestBodyWithSize(request)
	if err != nil {
		if domain.IsAudioTextAnalysis(request.AnalysisChain) {
			return Result{}, err
		}
		return Result{}, newError(ErrorKindRequest, 0, c.config.APIKey, "cannot encode request", err)
	}
	if len(body) > MaxRequestBytes {
		return Result{}, newError(ErrorKindRequest, 0, c.config.APIKey, "request body exceeds 2 MiB", nil)
	}
	result := Result{RequestID: request.RequestID, Usage: Usage{ImageBytes: imageBytes}}
	var lastErr *Error
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return Result{}, contextError(ctx.Err(), c.config.APIKey, attempt)
		}
		result.Usage.Attempts = attempt + 1
		httpRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint(), bytes.NewReader(body))
		if requestErr != nil {
			return Result{}, newError(ErrorKindRequest, 0, c.config.APIKey, "cannot create request", requestErr)
		}
		if c.config.APIKey != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+c.config.APIKey)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		if request.RequestID != "" {
			httpRequest.Header.Set("X-Request-ID", request.RequestID)
		}

		response, requestErr := c.httpClient.Do(httpRequest)
		if requestErr != nil {
			if ctx.Err() != nil {
				return Result{}, contextError(ctx.Err(), c.config.APIKey, attempt+1)
			}
			kind := ErrorKindTransport
			if c.config.Provider == ProviderLocal {
				kind = ErrorKindLocalModel
			}
			if errors.Is(requestErr, context.DeadlineExceeded) || os.IsTimeout(requestErr) {
				kind = ErrorKindTimeout
			}
			message := "provider request failed"
			cause := requestErr
			if c.config.Provider == ProviderLocal {
				cause = SanitizeError(requestErr, c.config.APIKey)
				if errors.Is(requestErr, errLocalEndpointRejected) {
					kind = ErrorKindLocalEndpoint
					message = "local endpoint rejected before request body"
				}
			}
			lastErr = newError(kind, 0, c.config.APIKey, message, cause)
			lastErr.Attempts = attempt + 1
			if (kind == ErrorKindTimeout && c.config.Provider != ProviderLocal) || attempt == c.config.MaxRetries || !retryableTransport(requestErr) {
				return Result{}, lastErr
			}
			if err := waitRetry(ctx, c.config.RetryDelay, attempt); err != nil {
				return Result{}, contextError(err, c.config.APIKey, attempt+1)
			}
			continue
		}

		status := response.StatusCode
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			kind := ErrorKindHTTP
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				kind = ErrorKindUnauthorized
			} else if status == http.StatusPaymentRequired {
				kind = ErrorKindQuota
			}
			lastErr = newError(kind, status, c.config.APIKey, "provider returned a non-success status", nil)
			lastErr.Attempts = attempt + 1
			if attempt == c.config.MaxRetries || !retryableStatus(status) || kind != ErrorKindHTTP {
				return Result{}, lastErr
			}
			if err := waitRetry(ctx, c.config.RetryDelay, attempt); err != nil {
				return Result{}, contextError(err, c.config.APIKey, attempt+1)
			}
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
		response.Body.Close()
		if readErr != nil {
			if c.config.Provider == ProviderLocal {
				if ctx.Err() != nil {
					return Result{}, contextError(ctx.Err(), c.config.APIKey, attempt+1)
				}
				if errors.Is(readErr, context.Canceled) {
					return Result{}, contextError(readErr, c.config.APIKey, attempt+1)
				}
				if errors.Is(readErr, errLocalEndpointRejected) {
					return Result{}, newError(ErrorKindLocalEndpoint, 0, c.config.APIKey, "local endpoint rejected while reading response", SanitizeError(readErr, c.config.APIKey))
				}
				if errors.Is(readErr, context.DeadlineExceeded) || os.IsTimeout(readErr) {
					lastErr = newError(ErrorKindTimeout, 0, c.config.APIKey, "local model response timed out", SanitizeError(readErr, c.config.APIKey))
					lastErr.Attempts = attempt + 1
					if attempt == c.config.MaxRetries {
						return Result{}, lastErr
					}
					if err := waitRetry(ctx, c.config.RetryDelay, attempt); err != nil {
						return Result{}, contextError(err, c.config.APIKey, attempt+1)
					}
					continue
				}
			}
			return Result{}, newError(ErrorKindProtocol, 0, c.config.APIKey, "cannot read provider response", readErr)
		}
		if len(data) > MaxResponseBytes {
			return Result{}, newError(ErrorKindProtocol, 0, c.config.APIKey, "provider response exceeds 1 MiB", nil)
		}
		if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
			return Result{}, newError(ErrorKindProtocol, 0, c.config.APIKey, "provider response must be JSON", nil)
		}
		if domain.IsAudioTextAnalysis(request.AnalysisChain) {
			parsed, parseErr := ParseTextResponse(data, request)
			if parseErr != nil {
				if classified, ok := parseErr.(*Error); ok {
					classified.message = sanitizeText(classified.message, c.config.APIKey)
					classified.cause = nil
					classified.Attempts = attempt + 1
					content, _, diagnostic, envelopeErr := parseResponseDiagnostic(data)
					if envelopeErr == nil {
						diagnostic.category = classifyContent([]byte(content))
					}
					classified.parse.category = diagnostic.category
					classified.parse.finishReason = diagnostic.finishReason
					classified.parse.contentLength = diagnostic.contentLength
				}
				return Result{}, parseErr
			}
			parsed.Usage.Attempts = result.Usage.Attempts
			parsed.Usage.ImageBytes = 0
			return parsed, nil
		}
		content, usage, diagnostic, parseErr := parseResponseDiagnostic(data)
		if parseErr != nil {
			if classified, ok := parseErr.(*Error); ok {
				classified.Attempts = attempt + 1
			}
			return Result{}, parseErr
		}
		events, parseErr := parseEventsDiagnostic(content, &diagnostic)
		if parseErr != nil {
			if classified, ok := parseErr.(*Error); ok {
				classified.message = sanitizeText(classified.message, c.config.APIKey)
				classified.cause = nil
				classified.Attempts = attempt + 1
				classified.parse.finishReason = diagnostic.finishReason
			}
			return Result{}, parseErr
		}
		if c.constrainedVision && diagnostic.invalidEventCount != 0 {
			classified := newError(ErrorKindProtocol, 0, "", "model content contains invalid events", nil)
			classified.Attempts = attempt + 1
			classified.parse = diagnostic
			return Result{}, classified
		}
		if c.constrainedVision && diagnostic.finishReason == FinishLength {
			classified := newError(ErrorKindProtocol, 0, "", "model response ended before event enumeration completed", nil)
			classified.Attempts = attempt + 1
			classified.parse = diagnostic
			return Result{}, classified
		}
		result.Events = events
		result.Usage.PromptTokens = usage.PromptTokens
		result.Usage.CompletionTokens = usage.CompletionTokens
		result.Usage.TotalTokens = usage.TotalTokens
		return result, nil
	}
	if lastErr != nil {
		return Result{}, lastErr
	}
	return Result{}, newError(ErrorKindTransport, 0, c.config.APIKey, "provider request failed", nil)
}

func retryableStatus(status int) bool { return status == http.StatusTooManyRequests || status >= 500 }
func retryableTransport(err error) bool {
	// Endpoint policy failures are deterministic and must not trigger another
	// connection attempt. Other transport failures use the same request ID,
	// provider, endpoint and model without relaxing response validation.
	return err != nil && !errors.Is(err, errLocalEndpointRejected)
}

func waitRetry(ctx context.Context, base time.Duration, attempt int) error {
	if base <= 0 {
		return nil
	}
	delay := base * time.Duration(1<<min(attempt, 5))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func contextError(err error, apiKey string, attempts int) *Error {
	kind := ErrorKindCanceled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorKindTimeout
	}
	classified := newError(kind, 0, apiKey, "request canceled or deadline exceeded", err)
	classified.Attempts = attempts
	return classified
}

func normalizeScoringRequest(request CandidateInput) (CandidateInput, error) {
	// The stream chain uses the exact snapshot thresholds and must not have
	// explicit values silently replaced by legacy defaults or floors.
	if request.AnalysisChain != domain.AnalysisChainAudioTextStreamV1 {
		if request.MinHighlightScore == 0 && request.MinCompletenessScore == 0 {
			request.MinHighlightScore = DefaultMinHighlightScore
			request.MinCompletenessScore = DefaultMinCompletenessScore
		}
	}
	if request.EpisodeNumber < 1 || request.CandidateIndex < 0 || !finite(request.StartSec) || !finite(request.EndSec) || request.StartSec < 0 || request.EndSec <= request.StartSec || !validScore(request.MinHighlightScore) || !validScore(request.MinCompletenessScore) {
		return CandidateInput{}, newError(ErrorKindRequest, 0, "", "invalid scoring request", nil)
	}
	if err := request.ValidateAnalysisInput(); err != nil {
		return CandidateInput{}, err
	}
	if request.AnalysisChain == domain.AnalysisChainAudioTextV1 &&
		(request.MinHighlightScore < DefaultMinHighlightScore || request.MinCompletenessScore < DefaultMinCompletenessScore) {
		return CandidateInput{}, newError(ErrorKindRequest, 0, "", "audio-text scoring thresholds cannot be lowered", nil)
	}
	if request.RequestID == "" {
		request.RequestID = fmt.Sprintf("E%03d-C%04d", request.EpisodeNumber, request.CandidateIndex)
	}
	return request, nil
}

type responseEnvelope struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason json.RawMessage `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func parseResponse(data []byte) (string, Usage, error) {
	content, usage, _, err := parseResponseDiagnostic(data)
	return content, usage, err
}

func parseResponseDiagnostic(data []byte) (string, Usage, parseDiagnostic, error) {
	var response responseEnvelope
	var diagnostic parseDiagnostic
	if err := decodeStrictJSON(data, &response); err != nil {
		return "", Usage{}, diagnostic, newError(ErrorKindProtocol, 0, "", "invalid Chat Completions response", nil)
	}
	if len(response.Choices) != 1 || len(response.Choices[0].Message.Content) == 0 {
		return "", Usage{}, diagnostic, newError(ErrorKindProtocol, 0, "", "response must contain exactly one choice with content", nil)
	}
	diagnostic.finishReason = allowedFinishReason(response.Choices[0].FinishReason)
	var content *string
	if err := json.Unmarshal(response.Choices[0].Message.Content, &content); err != nil || content == nil {
		classified := newError(ErrorKindProtocol, 0, "", "message.content must be a string", nil)
		classified.parse = diagnostic
		return "", Usage{}, diagnostic, classified
	}
	diagnostic.contentLength = len(*content)
	return *content, response.Usage, diagnostic, nil
}

func parseResponseContent(data []byte) (string, error) {
	content, _, err := parseResponse(data)
	return content, err
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Grammar     string        `json:"grammar,omitempty"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}
type imageURL struct {
	URL string `json:"url"`
}

func (c *Client) buildRequestBody(request CandidateInput) ([]byte, error) {
	body, _, err := c.buildRequestBodyWithSize(request)
	return body, err
}

func (c *Client) buildRequestBodyWithSize(request CandidateInput) ([]byte, int, error) {
	if domain.IsAudioTextAnalysis(request.AnalysisChain) {
		textRequest, err := BuildTextRequest(request, ModelConfig{Model: c.config.Model})
		if err != nil {
			return nil, 0, err
		}
		body, err := json.Marshal(textRequest)
		return body, 0, err
	}
	image, imageBytes, err := scoringImage(request, c.config.VisionEnabled)
	if err != nil {
		return nil, 0, err
	}
	prompt := BuildPromptWithImage(request, image != "")
	if c.constrainedVision {
		prompt = constrainedVisionPrompt(request, image != "")
	}
	var userContent any = prompt
	if image != "" {
		userContent = []contentPart{{Type: "text", Text: prompt}, {Type: "image_url", ImageURL: &imageURL{URL: image}}}
	}
	system := "你是短剧高光片段精评专家。请只返回严格 JSON，不要包含 Markdown、解释或额外文本。"
	var grammar string
	if c.constrainedVision {
		grammar = constrainedVisionGrammar(request)
		system = "You review candidate highlight events using the supplied evidence. Return one JSON object only. " +
			"List each distinct event exactly once. Include all supported distinct events, without a quota. " +
			"Do not repeat an event or restate it with another title. Do not invent events to fill the array. " +
			"After listing the supported events, close the events array and root object, then stop. " +
			"If no event is supported, return {\"events\":[]}. No Markdown or extra text."
	}
	body, err := json.Marshal(chatRequest{
		Model: c.config.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		},
		Temperature: 0.2,
		Grammar:     grammar,
	})
	return body, imageBytes, err
}

func scoringImage(request CandidateInput, visionEnabled bool) (string, int, error) {
	if !visionEnabled {
		return "", 0, nil
	}
	if request.Image != nil {
		if request.Image.MIMEType != "image/jpeg" || request.Image.DataBase64 == "" {
			return "", 0, fmt.Errorf("only non-empty JPEG image input is supported")
		}
		decoded, err := base64.StdEncoding.DecodeString(request.Image.DataBase64)
		if err != nil || len(decoded) == 0 || len(decoded) > maxContactSheetBytes {
			return "", 0, fmt.Errorf("invalid JPEG image input")
		}
		return "data:image/jpeg;base64," + request.Image.DataBase64, len(decoded), nil
	}
	if request.ContactSheetPath == "" {
		return "", 0, nil
	}
	info, err := os.Stat(request.ContactSheetPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxContactSheetBytes {
		return "", 0, nil
	}
	image, err := os.ReadFile(request.ContactSheetPath)
	if err != nil || len(image) == 0 {
		return "", 0, nil
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(image), len(image), nil
}

func BuildPrompt(request CandidateInput) string {
	return BuildPromptWithImage(request, request.ContactSheetPath != "" || request.Image != nil)
}

func BuildPromptWithImage(request CandidateInput, hasImage bool) string {
	contactSheetText := "未提供视觉输入"
	if hasImage {
		contactSheetText = "已提供 3x2 JPEG Contact Sheet"
	}
	transcript := request.Transcript
	if transcript == "" && len(request.EligibleUnits) > 0 {
		transcript = "可选语句（ID、源视频秒数、原文）：\n" + speechUnitLines(request.EligibleUnits)
	}
	contextText := request.Context
	if contextText == "" && len(request.ContextUnits) > 0 {
		contextText = "带时间的前后文（仅供理解）：\n" + speechUnitLines(request.ContextUnits)
	}
	return "目标：全面提取所有有吸引力的剧情，允许一集多段，不设每集或每个窗口的事件数量配额。\n" +
		"请逐一找出候选内值得保留的事件，返回严格 JSON 对象 {\"events\": [...]}。" +
		"没有符合标准的事件时返回 {\"events\": []}。同一窗口可有多个互不相同的事件；" +
		"不要只选最强的一个，不要把不连续事件拼成一段，也不要把同一连续事件拆成重复片段。\n" +
		"events 内每个事件必须包含：highlight_score、conflict_score、emotion_score、" +
		"reversal_score、visual_score、hook_score、completeness_score、" +
		"start_unit_id、end_unit_id、recommended_start_sec、recommended_end_sec、title、reason、tags、include_in_compilation。\n" +
		"所有评分字段取值范围为 0 到 1。\n" +
		"高光入选门槛是 highlight_score >= " + formatScore(request.MinHighlightScore) + "；" +
		"达到门槛表示有明确值得保留的吸引点，更高分表示吸引力更强。" +
		"吸引点包括冲突、反击、反转、身份揭露、机智对白、笑点、温情互动、情绪变化、" +
		"关系推进和有实质内容的悬念，不要求每段都有激烈冲突或核心爆点。" +
		"纯过渡、重复表达或没有吸引点的背景说明应排除。\n" +
		"completeness_score 必须 >= " + formatScore(request.MinCompletenessScore) + "，表示对白完整、当前事件可理解，" +
		"保留理解吸引点所需的铺垫和回应；不要求交代全部人物背景或全剧因果。" +
		"有内容的悬念可在完整一句或完整动作后结束，不必立即揭晓，但禁止把半句话当悬念。" +
		"需要回答才能表达吸引点的问答应保留关键回答。\n" +
		"每个事件的二次裁剪只能从可选语句列表选 start_unit_id 和 end_unit_id（含两端，中间连续保留）。" +
		"禁止编造 ID，禁止从词中间或半句开始/结束，禁止用秒数替代语句 ID；不要为凑时长硬切。" +
		"上下文仅供理解，不能选择只出现在上下文里的 ID。若需要超出可选范围才能完整表达，排除该候选。\n" +
		"仅当输入明确标记 VAD 确认整集无语音且实际提供图像时，才允许纯视觉评估，" +
		"此时两个 ID 返回 null，另返回 recommended_start_sec 和 recommended_end_sec。" +
		"拼版只能表示采样画面，不是连续视频；不得据此声称已测量音量、情绪或说话人。" +
		"没有足够证据判断动作完整性时应排除，不得编造声音或剧情。\n\n" +
		"请求 ID：" + request.RequestID + "\n" +
		"候选索引：" + strconv.Itoa(request.CandidateIndex) + "\n" +
		"剧集：第 " + strconv.Itoa(request.EpisodeNumber) + " 集\n" +
		"时间：" + strconv.FormatFloat(request.StartSec, 'f', -1, 64) + " 秒到 " + strconv.FormatFloat(request.EndSec, 'f', -1, 64) + " 秒\n" +
		"字幕：" + transcript + "\n" +
		"上下文：" + contextText + "\n" +
		"音频特征：" + request.AudioSummary + "\n" +
		"镜头特征：" + request.ShotSummary + "\n" +
		"视觉输入：" + contactSheetText + "\n"
}

func speechUnitLines(units []domain.SpeechUnit) string {
	lines := make([]string, 0, len(units))
	for _, unit := range units {
		lines = append(lines, unit.Line())
	}
	return strings.Join(lines, "\n")
}

func formatScore(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }

var _ Scorer = (*Client)(nil)
