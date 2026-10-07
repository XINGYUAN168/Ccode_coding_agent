package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/llmtransport"
)

// ResponsesClient 使用 OpenAI 官方 Responses API，并保留推理摘要和工具调用。
type ResponsesClient struct {
	client          openaisdk.Client
	model           string
	thinking        bool
	systemPrompt    string
	maxOutputTokens int
}

// NewResponses 构造 OpenAI 官方客户端。baseURL 非空时按完整 Responses endpoint
// 处理；空值使用 SDK 的官方默认地址。构造过程不会发起网络请求。
func NewResponses(name, model, apiKey, baseURL string, thinking bool, systemPrompt string, maxOutputTokens int, hc *http.Client) (*ResponsesClient, error) {
	name = strings.TrimSpace(name)
	model = strings.TrimSpace(model)
	switch {
	case name == "":
		return nil, llm.NewError(llm.ErrorUnknown, "OpenAI Provider 缺少名称", nil)
	case model == "":
		return nil, llm.NewError(llm.ErrorModel, "OpenAI Provider 缺少模型", nil)
	case strings.TrimSpace(apiKey) == "":
		return nil, llm.NewError(llm.ErrorAuth, "OpenAI Provider 缺少 api_key 或 OPENAI_API_KEY", nil)
	}

	var baseTransport http.RoundTripper
	if hc != nil {
		baseTransport = hc.Transport
	}
	httpClient := llmtransport.New(baseURL, baseTransport)
	return &ResponsesClient{
		client: openaisdk.NewClient(
			option.WithAPIKey(apiKey),
			option.WithHTTPClient(httpClient),
			option.WithMaxRetries(0),
		),
		model:           model,
		thinking:        thinking,
		systemPrompt:    systemPrompt,
		maxOutputTokens: maxOutputTokens,
	}, nil
}

func (c *ResponsesClient) SetSystemPrompt(prompt string) { c.systemPrompt = prompt }

func (c *ResponsesClient) SetMaxOutputTokens(tokens int) {
	if tokens > 0 {
		c.maxOutputTokens = tokens
	}
}

func (c *ResponsesClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent, 64)
	errs := make(chan error, 1)
	input := buildResponsesInput(conversation.EnsureToolPairing(conv.GetMessages()))
	tools := buildResponsesTools(toolSchemas)

	go func() {
		defer close(events)
		defer close(errs)

		params := responses.ResponseNewParams{
			Model:        shared.ResponsesModel(c.model),
			Instructions: param.NewOpt(c.systemPrompt),
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: input,
			},
		}
		if c.maxOutputTokens > 0 {
			params.MaxOutputTokens = param.NewOpt(int64(c.maxOutputTokens))
		}
		if c.thinking {
			params.Reasoning = shared.ReasoningParam{
				Effort:  shared.ReasoningEffortHigh,
				Summary: shared.ReasoningSummaryDetailed,
			}
			params.Include = []responses.ResponseIncludable{
				responses.ResponseIncludableReasoningEncryptedContent,
			}
		}
		if len(tools) > 0 {
			params.Tools = tools
		}

		stream := c.client.Responses.NewStreaming(ctx, params)
		defer stream.Close()

		type toolCall struct {
			id, name  string
			arguments strings.Builder
		}
		toolCalls := make(map[int64]*toolCall)
		reasoningIDs := make(map[int64]string)
		reasoningText := make(map[int64]*strings.Builder)
		completedToolCalls := 0
		ended := false

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "response.output_text.delta":
				if event.Delta != "" && !send(ctx, events, llm.TextDelta{Text: event.Delta}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.output_item.added":
				switch event.Item.Type {
				case "function_call":
					call := &toolCall{id: event.Item.CallID, name: event.Item.Name}
					toolCalls[event.OutputIndex] = call
					if !send(ctx, events, llm.ToolCallStart{ToolName: call.name, ToolID: call.id}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				case "reasoning":
					reasoningIDs[event.OutputIndex] = event.Item.ID
					reasoningText[event.OutputIndex] = &strings.Builder{}
				}
			case "response.reasoning_summary_text.delta":
				builder := reasoningText[event.OutputIndex]
				if builder == nil {
					builder = &strings.Builder{}
					reasoningText[event.OutputIndex] = builder
				}
				builder.WriteString(event.Delta)
				if event.Delta != "" && !send(ctx, events, llm.ThinkingDelta{Text: event.Delta}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.reasoning_summary_text.done":
				text := event.Text
				if builder := reasoningText[event.OutputIndex]; builder != nil && builder.Len() > 0 {
					text = builder.String()
				}
				if text != "" && !send(ctx, events, llm.ThinkingComplete{
					Thinking: text, Signature: reasoningIDs[event.OutputIndex],
				}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.function_call_arguments.delta":
				call := toolCalls[event.OutputIndex]
				if call == nil {
					call = &toolCall{}
					toolCalls[event.OutputIndex] = call
				}
				call.arguments.WriteString(event.Delta)
				if event.Delta != "" && !send(ctx, events, llm.ToolCallDelta{Text: event.Delta}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.function_call_arguments.done":
				call := toolCalls[event.OutputIndex]
				if call == nil {
					call = &toolCall{}
				}
				if call.arguments.Len() == 0 && event.Arguments != "" {
					call.arguments.WriteString(event.Arguments)
				}
				var arguments map[string]any
				raw := call.arguments.String()
				if strings.TrimSpace(raw) == "" {
					raw = "{}"
				}
				if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
					errs <- llm.Protocol(err)
					return
				}
				if arguments == nil {
					arguments = map[string]any{}
				}
				if !send(ctx, events, llm.ToolCallComplete{
					ToolID: call.id, ToolName: call.name, Arguments: arguments,
				}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
				completedToolCalls++
				delete(toolCalls, event.OutputIndex)
			case "response.completed":
				ended = true
				stopReason := "end_turn"
				if completedToolCalls > 0 {
					stopReason = "tool_use"
				}
				if !send(ctx, events, llm.StreamEnd{StopReason: stopReason, Usage: responsesUsage(event.Response)}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.incomplete":
				ended = true
				stopReason := "end_turn"
				if event.Response.IncompleteDetails.Reason == "max_output_tokens" {
					stopReason = "max_tokens"
				}
				if !send(ctx, events, llm.StreamEnd{StopReason: stopReason, Usage: responsesUsage(event.Response)}) {
					nonBlockingError(errs, llm.Classify(ctx.Err()))
					return
				}
			case "response.failed", "error":
				errs <- llm.Protocol(errors.New("OpenAI Responses API 返回失败事件"))
				return
			}
		}

		if err := stream.Err(); err != nil {
			errs <- classifyError(err)
			return
		}
		if !ended {
			errs <- llm.Protocol(io.ErrUnexpectedEOF)
		}
	}()

	return events, errs
}

func buildResponsesTools(schemas []map[string]any) []responses.ToolUnionParam {
	result := make([]responses.ToolUnionParam, 0, len(schemas))
	for _, schema := range schemas {
		name, _ := schema["name"].(string)
		description, _ := schema["description"].(string)
		parameters, _ := schema["parameters"].(map[string]any)
		result = append(result, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name: name, Description: param.NewOpt(description), Parameters: parameters,
				Strict: param.NewOpt(false),
			},
		})
	}
	return result
}

func buildResponsesInput(messages []conversation.Message) responses.ResponseInputParam {
	input := make(responses.ResponseInputParam, 0, len(messages))
	for _, message := range messages {
		switch {
		case message.Role == "assistant":
			for _, block := range message.ThinkingBlocks {
				input = append(input, responses.ResponseInputItemParamOfReasoning(
					block.Signature,
					[]responses.ResponseReasoningItemSummaryParam{{Text: block.Thinking}},
				))
			}
			if message.Content != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(
					message.Content, responses.EasyInputMessageRoleAssistant,
				))
			}
			for _, toolUse := range message.ToolUses {
				arguments, err := json.Marshal(toolUse.Arguments)
				if err != nil {
					arguments = []byte("{}")
				}
				input = append(input, responses.ResponseInputItemParamOfFunctionCall(
					string(arguments), toolUse.ToolUseID, toolUse.ToolName,
				))
			}
		case len(message.ToolResults) > 0:
			for _, result := range message.ToolResults {
				item := responses.ResponseInputItemParamOfFunctionCallOutput(result.Content)
				item.OfFunctionCallOutput.CallID = param.NewOpt(result.ToolUseID)
				input = append(input, item)
			}
		default:
			input = append(input, responses.ResponseInputItemParamOfMessage(
				message.Content, responses.EasyInputMessageRoleUser,
			))
		}
	}
	return input
}

func responsesUsage(response responses.Response) llm.UsageInfo {
	cached := int(response.Usage.InputTokensDetails.CachedTokens)
	input := int(response.Usage.InputTokens) - cached
	if input < 0 {
		input = 0
	}
	return llm.UsageInfo{
		InputTokens:         input,
		OutputTokens:        int(response.Usage.OutputTokens),
		CacheReadTokens:     cached,
		CacheCreationTokens: int(response.Usage.InputTokensDetails.CacheWriteTokens),
	}
}
