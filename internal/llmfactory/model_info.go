package llmfactory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ccode/internal/config"
	"ccode/internal/llm"
	"ccode/internal/llmtransport"
)

const modelInfoTimeout = 2 * time.Second
const maxModelsBodyBytes = 1 << 20

var defaultChatEndpoints = map[string]string{
	config.ProtocolAnthropic:    "https://api.anthropic.com/v1/messages",
	config.ProtocolOpenAI:       "https://api.openai.com/v1/responses",
	config.ProtocolOpenAICompat: "https://api.openai.com/v1/chat/completions",
}

var ErrModelInfoUnavailable = errors.New("模型信息不可用，将使用配置值")

type modelItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	MaxInputTokens int    `json:"max_input_tokens"`
}

type modelsListResponse struct {
	Data []modelItem `json:"data"`
}

// ResolveModelInfo 对可识别的标准聊天端点执行短时、尽力而为的模型列表查询。
func (f *Factory) ResolveModelInfo(ctx context.Context, cfg config.ProviderConfig) (llm.ModelInfo, error) {
	endpoint := strings.TrimSpace(cfg.BaseURL)
	if endpoint == "" {
		endpoint = defaultChatEndpoints[cfg.Protocol]
	}
	modelsURL, ok := deriveModelsURL(cfg.Protocol, endpoint)
	if !ok {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}

	requestContext, cancel := context.WithTimeout(ctx, modelInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, modelsURL, nil)
	if err != nil {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		req.Header.Set("x-api-key", cfg.ResolveAPIKey())
		req.Header.Set("anthropic-version", "2023-06-01")
	case config.ProtocolOpenAI, config.ProtocolOpenAICompat:
		req.Header.Set("Authorization", "Bearer "+cfg.ResolveAPIKey())
	default:
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}

	client := llmtransport.New("", f.opts.BaseTransport)
	resp, err := client.Do(req)
	if err != nil {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if err != nil || len(body) > maxModelsBodyBytes {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}
	item, ok := matchModelItem(body, cfg.Model)
	if !ok {
		return llm.ModelInfo{}, ErrModelInfoUnavailable
	}
	window := item.MaxInputTokens
	if window < 0 {
		window = 0
	}
	return llm.ModelInfo{
		ID: item.ID, DisplayName: chooseDisplayName(item), Provider: cfg.Name, ContextWindow: window,
	}, nil
}

func deriveModelsURL(protocol, chatEndpoint string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(chatEndpoint))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	switch protocol {
	case config.ProtocolAnthropic:
		if !strings.HasSuffix(u.Path, "/messages") {
			return "", false
		}
		u.Path = strings.TrimSuffix(u.Path, "/messages") + "/models"
	case config.ProtocolOpenAI:
		if !strings.HasSuffix(u.Path, "/responses") {
			return "", false
		}
		u.Path = strings.TrimSuffix(u.Path, "/responses") + "/models"
	case config.ProtocolOpenAICompat:
		if !strings.HasSuffix(u.Path, "/chat/completions") {
			return "", false
		}
		u.Path = strings.TrimSuffix(u.Path, "/chat/completions") + "/models"
	default:
		return "", false
	}
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.User = nil
	return u.String(), true
}

func matchModelItem(body []byte, model string) (modelItem, bool) {
	var list modelsListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return modelItem{}, false
	}
	for _, item := range list.Data {
		if item.ID == model {
			return item, true
		}
	}
	return modelItem{}, false
}

func chooseDisplayName(item modelItem) string {
	if item.DisplayName != "" {
		return item.DisplayName
	}
	if item.Name != "" {
		return item.Name
	}
	return item.ID
}
