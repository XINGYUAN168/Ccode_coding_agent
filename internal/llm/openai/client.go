// Package openai 提供 OpenAI Responses 与兼容 Chat Completions 两种客户端。
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/llmtransport"
)

// Client 服务兼容 OpenAI Chat Completions 的第三方端点。
type Client struct {
	client       openaisdk.Client
	model        string
	systemPrompt string
}

// New 保留原有构造入口，实际创建 Chat Completions 兼容客户端。
func New(name, model, apiKey, baseURL string, thinking bool, systemPrompt string, hc *http.Client) (*Client, error) {
	_ = thinking
	return NewCompat(name, model, apiKey, baseURL, systemPrompt, hc)
}

// NewCompat 使用显式配置构造 Chat Completions 兼容客户端。构造过程不会发起网络请求。
func NewCompat(name, model, apiKey, baseURL, systemPrompt string, hc *http.Client) (*Client, error) {
	name = strings.TrimSpace(name)
	model = strings.TrimSpace(model)
	switch {
	case name == "":
		return nil, llm.NewError(llm.ErrorUnknown, "OpenAI Provider 缺少名称", nil)
	case model == "":
		return nil, llm.NewError(llm.ErrorModel, "OpenAI Provider 缺少模型", nil)
	case strings.TrimSpace(apiKey) == "":
		return nil, llm.NewError(llm.ErrorAuth, "OpenAI Provider 缺少 api_key", nil)
	}
	var baseTransport http.RoundTripper
	if hc != nil {
		baseTransport = hc.Transport
	}
	httpClient := llmtransport.New(baseURL, baseTransport)
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if strings.TrimSpace(baseURL) != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Client{
		client:       openaisdk.NewClient(opts...),
		model:        model,
		systemPrompt: systemPrompt,
	}, nil
}

func (c *Client) SetSystemPrompt(prompt string) { c.systemPrompt = prompt }

func (c *Client) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent, 64)
	errs := make(chan error, 1)
	messages := buildMessages(c.systemPrompt, conversation.EnsureToolPairing(conv.GetMessages()))
	tools := buildTools(toolSchemas)

	go func() {
		defer close(events)
		defer close(errs)

		params := openaisdk.ChatCompletionNewParams{
			Model:    openaisdk.ChatModel(c.model),
			Messages: messages,
			StreamOptions: openaisdk.ChatCompletionStreamOptionsParam{
				IncludeUsage: openaisdk.Bool(true),
			},
		}
		if len(tools) > 0 {
			params.Tools = tools
		}

		stream := c.client.Chat.Completions.NewStreaming(ctx, params)
		defer stream.Close()
		var acc openaisdk.ChatCompletionAccumulator
		finishReason := ""
		var reasoningAccum strings.Builder
		started := make(map[int64]bool)

		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			for _, choice := range chunk.Choices {
				if choice.FinishReason != "" {
					finishReason = choice.FinishReason
				}
				if choice.Delta.Content != "" && !send(ctx, events, llm.TextDelta{Text: choice.Delta.Content}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
				if reasoning := reasoningContent(choice.Delta); reasoning != "" {
					reasoningAccum.WriteString(reasoning)
					if !send(ctx, events, llm.ThinkingDelta{Text: reasoning}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				}
				// refusal、role、audio 等不属于当前统一事件契约的扩展字段保持忽略。
				for _, call := range choice.Delta.ToolCalls {
					if !started[call.Index] && (call.ID != "" || call.Function.Name != "") {
						started[call.Index] = true
						if !send(ctx, events, llm.ToolCallStart{ToolName: call.Function.Name, ToolID: call.ID}) {
							nonBlockingError(errs, llm.Classify(ctx.Err()))
							return
						}
					}
					if call.Function.Arguments != "" && !send(ctx, events, llm.ToolCallDelta{Text: call.Function.Arguments}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			errs <- classifyError(err)
			return
		}
		if finishReason == "" {
			errs <- llm.Protocol(io.ErrUnexpectedEOF)
			return
		}
		if reasoningAccum.Len() > 0 {
			if !send(ctx, events, llm.ThinkingComplete{Thinking: reasoningAccum.String()}) {
				nonBlockingError(errs, llm.Classify(ctx.Err()))
				return
			}
		}

		for _, call := range collectToolCalls(&acc) {
			var args map[string]any
			if err := json.Unmarshal([]byte(call.arguments), &args); err != nil {
				errs <- llm.Protocol(err)
				return
			}
			if args == nil {
				args = map[string]any{}
			}
			if !send(ctx, events, llm.ToolCallComplete{ToolID: call.id, ToolName: call.name, Arguments: args}) {
				nonBlockingError(errs, llm.Classify(ctx.Err()))
				return
			}
		}

		cached := int(acc.Usage.PromptTokensDetails.CachedTokens)
		input := int(acc.Usage.PromptTokens) - cached
		if input < 0 {
			input = 0
		}
		stopReason := "end_turn"
		switch finishReason {
		case "tool_calls":
			stopReason = "tool_use"
		case "length":
			stopReason = "max_tokens"
		}
		if !send(ctx, events, llm.StreamEnd{
			StopReason: stopReason,
			Usage: llm.UsageInfo{
				InputTokens:     input,
				OutputTokens:    int(acc.Usage.CompletionTokens),
				CacheReadTokens: cached,
			},
		}) {
			nonBlockingError(errs, llm.Classify(ctx.Err()))
		}
	}()

	return events, errs
}

func reasoningContent(delta openaisdk.ChatCompletionChunkChoiceDelta) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(delta.RawJSON()), &fields); err != nil {
		return ""
	}
	raw, ok := fields["reasoning_content"]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return text
}

type completeToolCall struct {
	id        string
	name      string
	arguments string
}

func collectToolCalls(acc *openaisdk.ChatCompletionAccumulator) []completeToolCall {
	var calls []completeToolCall
	for i := range acc.Choices {
		for _, call := range acc.Choices[i].Message.ToolCalls {
			if call.Type != "" && call.Type != "function" {
				continue
			}
			if call.ID == "" && call.Function.Name == "" {
				continue
			}
			args := call.Function.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, completeToolCall{id: call.ID, name: call.Function.Name, arguments: args})
		}
	}
	return calls
}

func buildMessages(systemPrompt string, messages []conversation.Message) []openaisdk.ChatCompletionMessageParamUnion {
	result := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if systemPrompt != "" {
		result = append(result, openaisdk.SystemMessage(systemPrompt))
	}
	for _, message := range messages {
		switch {
		case message.Role == "assistant":
			assistant := openaisdk.ChatCompletionAssistantMessageParam{}
			if message.Content != "" {
				assistant.Content.OfString = param.NewOpt(message.Content)
			}
			var reasoning strings.Builder
			for _, block := range message.ThinkingBlocks {
				reasoning.WriteString(block.Thinking)
			}
			if reasoning.Len() > 0 {
				assistant.SetExtraFields(map[string]any{"reasoning_content": reasoning.String()})
			}
			for _, toolUse := range message.ToolUses {
				arguments, err := json.Marshal(toolUse.Arguments)
				if err != nil {
					arguments = []byte("{}")
				}
				assistant.ToolCalls = append(assistant.ToolCalls, openaisdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
						ID: toolUse.ToolUseID,
						Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: toolUse.ToolName, Arguments: string(arguments),
						},
					},
				})
			}
			result = append(result, openaisdk.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case len(message.ToolResults) > 0:
			for _, toolResult := range message.ToolResults {
				result = append(result, openaisdk.ToolMessage(toolResult.Content, toolResult.ToolUseID))
			}
		default:
			result = append(result, openaisdk.UserMessage(message.Content))
		}
	}
	return result
}

func buildTools(schemas []map[string]any) []openaisdk.ChatCompletionToolUnionParam {
	result := make([]openaisdk.ChatCompletionToolUnionParam, 0, len(schemas))
	for _, schema := range schemas {
		name, _ := schema["name"].(string)
		description, _ := schema["description"].(string)
		parameters, _ := schema["parameters"].(map[string]any)
		result = append(result, openaisdk.ChatCompletionFunctionTool(openaisdk.FunctionDefinitionParam{
			Name: name, Description: openaisdk.String(description), Parameters: openaisdk.FunctionParameters(parameters),
		}))
	}
	return result
}

func classifyError(err error) error {
	var apiErr *openaisdk.Error
	if errors.As(err, &apiErr) {
		retry := ""
		if apiErr.Response != nil {
			retry = apiErr.Response.Header.Get("Retry-After")
		}
		return llm.HTTPStatus(apiErr.StatusCode, apiErr.Message, err, retry)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return llm.Classify(err)
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return llm.Network(err)
	}
	return llm.Protocol(err)
}

func send(ctx context.Context, events chan<- llm.StreamEvent, event llm.StreamEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func nonBlockingError(errs chan<- error, err error) {
	select {
	case errs <- err:
	default:
	}
}
