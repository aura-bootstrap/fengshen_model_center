package scoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ProviderLocal    = "local"
	ProviderVolcano  = "volcano"
	ProviderDeepSeek = "deepseek"

	LocalBaseURL    = "http://127.0.0.1:11434/v1"
	VolcanoBaseURL  = "https://ark.cn-beijing.volces.com/api/v3"
	DeepSeekBaseURL = "https://api.deepseek.com/v1"
	DeepSeekModel   = "deepseek-flash"

	DefaultTimeoutSec = 120
	MaxTimeoutSec     = 1800
	DefaultMaxRetries = 2
)

// Config contains OpenAI-compatible provider settings. APIKey is never
// serialized. MaxRetries and RetryDelay control same-provider retries only.
type Config struct {
	Provider       string        `json:"provider"`
	BaseURL        string        `json:"base_url"`
	APIKey         string        `json:"-"`
	Model          string        `json:"model"`
	TimeoutSec     int           `json:"timeout_sec"`
	VisionEnabled  bool          `json:"vision_enabled"`
	MaxRetries     int           `json:"-"`
	DisableRetries bool          `json:"-"`
	RetryDelay     time.Duration `json:"-"`
}

type fileConfig struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	Model         string `json:"model"`
	TimeoutSec    int    `json:"timeout_sec"`
	VisionEnabled *bool  `json:"vision_enabled"`
}

// DefaultConfig returns the legacy Volcano preset. A Volcano model/endpoint ID
// remains explicitly required; DeepSeek and local are selected only explicitly.
func DefaultConfig() Config {
	return WithDefaults(Config{VisionEnabled: true})
}

// DefaultLocalConfig returns the loopback preset for a new local configuration.
// The model is deliberately empty: callers must require an explicit model name.
func DefaultLocalConfig() Config {
	return WithDefaults(Config{Provider: ProviderLocal})
}

func WithDefaults(cfg Config) Config { return withConfigDefaults(cfg) }

// LoadConfig supports the existing local development configuration contract.
// Server deployments should inject secrets directly rather than reading this file.
func LoadConfig(path string) (Config, error) {
	var raw fileConfig
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, newError(ErrorKindConfig, 0, os.Getenv("MYCLIP_AI_API_KEY"), "cannot read configuration", err)
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return Config{}, newError(ErrorKindConfig, 0, os.Getenv("MYCLIP_AI_API_KEY"), "invalid configuration JSON", err)
		}
	}
	cfg := Config{Provider: raw.Provider, BaseURL: raw.BaseURL, APIKey: raw.APIKey, Model: raw.Model, TimeoutSec: raw.TimeoutSec, VisionEnabled: true}
	if raw.VisionEnabled != nil {
		cfg.VisionEnabled = *raw.VisionEnabled
	}
	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	cfg = withConfigDefaults(cfg)
	if cfg.Provider == ProviderLocal && raw.VisionEnabled == nil && strings.TrimSpace(os.Getenv("MYCLIP_AI_VISION_ENABLED")) == "" {
		cfg.VisionEnabled = false
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvironment(cfg *Config) error {
	setStringFromEnv(&cfg.Provider, "MYCLIP_AI_PROVIDER")
	setStringFromEnv(&cfg.BaseURL, "MYCLIP_AI_BASE_URL")
	setStringFromEnv(&cfg.APIKey, "MYCLIP_AI_API_KEY")
	setStringFromEnv(&cfg.Model, "MYCLIP_AI_MODEL")
	if value := strings.TrimSpace(os.Getenv("MYCLIP_AI_VISION_ENABLED")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return newError(ErrorKindConfig, 0, cfg.APIKey, "MYCLIP_AI_VISION_ENABLED must be a boolean", nil)
		}
		cfg.VisionEnabled = parsed
	}
	return nil
}

func setStringFromEnv(target *string, name string) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		*target = strings.TrimSpace(value)
	}
}

func withConfigDefaults(cfg Config) Config {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = ProviderVolcano
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.BaseURL == "" {
		switch cfg.Provider {
		case ProviderLocal:
			cfg.BaseURL = LocalBaseURL
		case ProviderDeepSeek:
			cfg.BaseURL = DeepSeekBaseURL
		default:
			cfg.BaseURL = VolcanoBaseURL
		}
	}
	if cfg.Model == "" && cfg.Provider == ProviderDeepSeek {
		cfg.Model = DeepSeekModel
	}
	if cfg.TimeoutSec == 0 {
		cfg.TimeoutSec = DefaultTimeoutSec
	}
	if cfg.DisableRetries {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 25 * time.Millisecond
	}
	return cfg
}

func (c Config) Validate() error {
	switch c.Provider {
	case ProviderLocal, ProviderVolcano, ProviderDeepSeek:
	default:
		return newError(ErrorKindConfig, 0, c.APIKey, "provider must be local, volcano, or deepseek", nil)
	}
	if c.Provider != ProviderLocal && c.APIKey == "" {
		return newError(ErrorKindConfig, 0, c.APIKey, "API key is required", nil)
	}
	configErrorKind := ErrorKindConfig
	if c.Provider == ProviderLocal {
		configErrorKind = ErrorKindLocalModelConfig
	}
	if c.Model == "" {
		message := "model or volcano endpoint ID is required"
		if c.Provider == ProviderLocal {
			message = "explicit local model is required"
		}
		return newError(configErrorKind, 0, c.APIKey, message, nil)
	}
	if c.TimeoutSec <= 0 || c.TimeoutSec > MaxTimeoutSec {
		return newError(configErrorKind, 0, c.APIKey, "timeout_sec must be between 1 and 1800", nil)
	}
	if c.MaxRetries < 0 || c.MaxRetries > 5 || c.RetryDelay < 0 {
		return newError(configErrorKind, 0, c.APIKey, "retry settings are invalid", nil)
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return newError(configErrorKind, 0, c.APIKey, "base_url must be an absolute HTTP(S) URL without credentials, query, or fragment", nil)
	}
	if c.Provider == ProviderLocal {
		if err := validateLocalBaseURL(c.BaseURL); err != nil {
			return newError(ErrorKindLocalModelConfig, 0, c.APIKey, "local base_url must use an exact loopback host without credentials, query, or fragment", err)
		}
	} else {
		if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
			return newError(ErrorKindConfig, 0, c.APIKey, "plain HTTP is allowed only for an explicit loopback mock", nil)
		}
		if c.Provider == ProviderDeepSeek && c.VisionEnabled && (strings.EqualFold(c.Model, "deepseek-chat") || strings.EqualFold(c.Model, "deepseek-reasoner")) {
			return newError(ErrorKindConfig, 0, c.APIKey, "DeepSeek text models require vision_enabled=false", nil)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Config) Endpoint() string { return strings.TrimRight(c.BaseURL, "/") + "/chat/completions" }

func (c Config) String() string {
	return fmt.Sprintf("{Provider:%s BaseURL:%s APIKey:[REDACTED] Model:%s TimeoutSec:%d VisionEnabled:%t}", c.Provider, c.BaseURL, c.Model, c.TimeoutSec, c.VisionEnabled)
}
func (c Config) GoString() string { return c.String() }

func (c Config) MarshalJSON() ([]byte, error) {
	type safeConfig struct {
		Provider      string `json:"provider"`
		BaseURL       string `json:"base_url"`
		APIKey        string `json:"api_key"`
		Model         string `json:"model"`
		TimeoutSec    int    `json:"timeout_sec"`
		VisionEnabled bool   `json:"vision_enabled"`
	}
	return json.Marshal(safeConfig{c.Provider, c.BaseURL, "[REDACTED]", c.Model, c.TimeoutSec, c.VisionEnabled})
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
