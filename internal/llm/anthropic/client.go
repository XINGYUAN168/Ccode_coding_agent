package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ccode/internal/conversation"
	"ccode/internal/llm"
	"ccode/internal/llmtransport"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicStreamIdleTimeout = 5 * time.Minute

// nativeToolSearchBeta 开启 defer_loading 和 tool_reference。跟
// mcp.NativeToolSearchBeta 是同一个值，这里单独定义以免 llm 反向依赖 mcp。
const nativeToolSearchBeta = "advanced-tool-use-2025-11-20"

// markToolsForCache 把缓存断点打在最后一个非延迟工具上。
//
// tool schema 在多轮之间稳定，标记尾部就能把整个工具块缓存下来，几乎是免费的。但
// 断点不能落在带 defer_loading 的工具上：一个工具同时带 defer_loading 和
// cache_control 会被官方端点直接拒掉整个请求。MCP 工具在内建工具之后注册，排序后
// 尾部往往正是延迟工具，所以必须从尾部往前找。内建工具永远不延迟，总能找到落点。
func markToolsForCache(sdkTools []anthropicsdk.ToolUnionParam) {
	for i := len(sdkTools) - 1; i >= 0; i-- {
		t := sdkTools[i].OfTool
		if t == nil || t.DeferLoading.Valid() {
			continue
		}
		t.CacheControl = anthropicsdk.NewCacheControlEphemeralParam()
		return
	}
}

// needsToolSearchBeta 判断这批工具里有没有带 defer_loading 的。
//
// 只在真用到时才发这个 beta header：不认识它的端点收到会直接拒请求，而
// dispatch / eager 两条路压根不需要它。
func needsToolSearchBeta(toolSchemas []map[string]any) bool {
	for _, s := range toolSchemas {
		if deferLoading, _ := s["defer_loading"].(bool); deferLoading {
			return true
		}
	}
	return false
}

// supportsAdaptiveThinking 判断模型是否支持 Anthropic 自适应思考模式。
func supportsAdaptiveThinking(model string) bool {
	for _, family := range []string{"claude-opus-4-", "claude-sonnet-4-"} {
		if !strings.HasPrefix(model, family) {
			continue
		}
		rest := model[len(family):]
		return len(rest) > 0 && rest[0] >= '6' && rest[0] <= '9'
	}
	return false
}

type Client struct {
	client          anthropicsdk.Client
	model           string
	thinking        bool
	systemPrompt    string
	maxOutputTokens int
}

// New 使用显式配置构造 Anthropic 客户端。构造过程只装配 SDK 和传输，不发起网络请求。
func New(name, model, apiKey, baseURL string, thinking bool, systemPrompt string, maxOutputTokens int, hc *http.Client) (*Client, error) {
	name = strings.TrimSpace(name)
	model = strings.TrimSpace(model)
	if name == "" || model == "" || strings.TrimSpace(apiKey) == "" {
		return nil, llm.NewError(llm.ErrorAuth, "Anthropic Provider 缺少必填配置", nil)
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = 8192
	}
	if hc == nil {
		hc = llmtransport.New(baseURL, nil)
	}
	client := anthropicsdk.NewClient(
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(hc),
		option.WithMaxRetries(0),
	)
	return &Client{
		client:          client,
		model:           model,
		thinking:        thinking,
		systemPrompt:    systemPrompt,
		maxOutputTokens: maxOutputTokens,
	}, nil
}

func (c *Client) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

func (c *Client) SetMaxOutputTokens(tokens int) {
	c.maxOutputTokens = tokens
}

func (c *Client) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent, 64)
	errs := make(chan error, 1)

	// 发请求前补齐工具调用与结果的配对：中断、恢复会话、并发交错都可能留下
	// 悬空的 tool_use，缺配对会被 API 直接拒掉。
	history := conversation.EnsureToolPairing(conv.GetMessages())
	msgs := buildAnthropicMessages(history)

	var sdkTools []anthropicsdk.ToolUnionParam
	// 带 defer_loading 的工具留在 tools[] 里但服务端不给模型看，模型要先用
	// ToolSearch 拿 tool_reference 才能调。这个字段需要 beta header 才被接受。
	sendToolSearchBeta := needsToolSearchBeta(toolSchemas)
	for _, s := range toolSchemas {
		inputSchema, _ := s["input_schema"].(map[string]any)
		props, _ := inputSchema["properties"]
		required, _ := inputSchema["required"].([]string)
		desc, _ := s["description"].(string)
		tool := &anthropicsdk.ToolParam{
			Name:        s["name"].(string),
			Description: param.NewOpt(desc),
			InputSchema: anthropicsdk.ToolInputSchemaParam{
				Properties: props,
				Required:   required,
			},
		}
		if deferLoading, _ := s["defer_loading"].(bool); deferLoading {
			tool.DeferLoading = param.NewOpt(true)
		}
		sdkTools = append(sdkTools, anthropicsdk.ToolUnionParam{OfTool: tool})
	}

	go func() {
		defer close(events)
		defer close(errs)

		maxTokens := int64(c.maxOutputTokens)
		// 在最长且稳定的系统提示前缀、工具列表和最后一条用户消息末尾设置缓存断点。
		params := anthropicsdk.MessageNewParams{
			Model:     c.model,
			MaxTokens: maxTokens,
			System: []anthropicsdk.TextBlockParam{{
				Text:         c.systemPrompt,
				CacheControl: anthropicsdk.NewCacheControlEphemeralParam(),
			}},
			Messages: msgs,
		}
		markLastUserTailForCache(params.Messages)
		if c.thinking {
			if supportsAdaptiveThinking(c.model) {
				params.Thinking = anthropicsdk.ThinkingConfigParamUnion{
					OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{},
				}
			} else {
				params.Thinking = anthropicsdk.ThinkingConfigParamUnion{
					OfEnabled: &anthropicsdk.ThinkingConfigEnabledParam{
						BudgetTokens: maxTokens - 1,
					},
				}
			}
		}
		if len(sdkTools) > 0 {
			markToolsForCache(sdkTools)
			params.Tools = sdkTools
		}

		var reqOpts []option.RequestOption
		if sendToolSearchBeta {
			reqOpts = append(reqOpts, option.WithHeaderAdd("anthropic-beta", nativeToolSearchBeta))
		}

		stream := c.client.Messages.NewStreaming(ctx, params, reqOpts...)
		defer stream.Close()

		var currentToolName, currentToolID, jsonAccum string
		var thinkingAccum, thinkingSignature string
		inThinking := false
		var accMessage anthropicsdk.Message
		sawStop := false

		// 在独立 goroutine 中读取 SSE，以便同时响应调用方取消和空闲超时。
		type sseResult struct {
			hasNext bool
		}
		nextCh := make(chan sseResult, 1)

		readNext := func() {
			nextCh <- sseResult{hasNext: stream.Next()}
		}

		idle := time.NewTimer(anthropicStreamIdleTimeout)
		defer idle.Stop()

		go readNext()
		for {
			var res sseResult
			select {
			case <-ctx.Done():
				errs <- llm.Classify(ctx.Err())
				return
			case <-idle.C:
				errs <- llm.Network(context.DeadlineExceeded)
				return
			case res = <-nextCh:
			}

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(anthropicStreamIdleTimeout)

			if !res.hasNext {
				break
			}

			event := stream.Current()
			if err := accMessage.Accumulate(event); err != nil {
				errs <- llm.Protocol(err)
				return
			}
			if event.Type == "message_stop" {
				sawStop = true
			}
			// 某些兼容服务会在 message_delta 中补充输入与缓存用量，这里显式合并。
			if mde, ok := event.AsAny().(anthropicsdk.MessageDeltaEvent); ok {
				if mde.Usage.InputTokens > 0 {
					accMessage.Usage.InputTokens = mde.Usage.InputTokens
				}
				if mde.Usage.CacheReadInputTokens > 0 {
					accMessage.Usage.CacheReadInputTokens = mde.Usage.CacheReadInputTokens
				}
				if mde.Usage.CacheCreationInputTokens > 0 {
					accMessage.Usage.CacheCreationInputTokens = mde.Usage.CacheCreationInputTokens
				}
			}
			switch ev := event.AsAny().(type) {
			case anthropicsdk.ContentBlockStartEvent:
				switch ev.ContentBlock.Type {
				case "thinking":
					inThinking = true
					thinkingAccum = ""
					thinkingSignature = ""
				case "tool_use":
					currentToolName = ev.ContentBlock.Name
					currentToolID = ev.ContentBlock.ID
					jsonAccum = ""
					if !send(ctx, events, llm.ToolCallStart{ToolName: currentToolName, ToolID: currentToolID}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				}
			case anthropicsdk.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropicsdk.ThinkingDelta:
					thinkingAccum += delta.Thinking
					if !send(ctx, events, llm.ThinkingDelta{Text: delta.Thinking}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				case anthropicsdk.SignatureDelta:
					thinkingSignature += delta.Signature
				case anthropicsdk.TextDelta:
					if !send(ctx, events, llm.TextDelta{Text: delta.Text}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				case anthropicsdk.InputJSONDelta:
					jsonAccum += delta.PartialJSON
					if !send(ctx, events, llm.ToolCallDelta{Text: delta.PartialJSON}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
				}
			case anthropicsdk.ContentBlockStopEvent:
				if inThinking {
					if !send(ctx, events, llm.ThinkingComplete{
						Thinking:  thinkingAccum,
						Signature: thinkingSignature,
					}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
					inThinking = false
				}
				if currentToolName != "" {
					var args map[string]any
					if jsonAccum != "" {
						if err := json.Unmarshal([]byte(jsonAccum), &args); err != nil {
							errs <- llm.Protocol(err)
							return
						}
					}
					if args == nil {
						args = map[string]any{}
					}
					if !send(ctx, events, llm.ToolCallComplete{
						ToolID:    currentToolID,
						ToolName:  currentToolName,
						Arguments: args,
					}) {
						nonBlockingError(errs, llm.Classify(ctx.Err()))
						return
					}
					currentToolName = ""
					currentToolID = ""
					jsonAccum = ""
				}
			}

			go readNext()
		}

		if err := stream.Err(); err != nil {
			errs <- classifyAnthropicError(err)
			return
		}
		if !sawStop {
			errs <- llm.Protocol(io.ErrUnexpectedEOF)
			return
		}

		stopReason := string(accMessage.StopReason)
		if stopReason == "" {
			stopReason = "end_turn"
		}
		usage := llm.UsageInfo{
			InputTokens:         int(accMessage.Usage.InputTokens),
			OutputTokens:        int(accMessage.Usage.OutputTokens),
			CacheReadTokens:     int(accMessage.Usage.CacheReadInputTokens),
			CacheCreationTokens: int(accMessage.Usage.CacheCreationInputTokens),
		}
		if !send(ctx, events, llm.StreamEnd{StopReason: stopReason, Usage: usage}) {
			nonBlockingError(errs, llm.Classify(ctx.Err()))
		}
	}()

	return events, errs
}

// markLastUserTailForCache 在最后一条用户消息的末尾内容块设置临时缓存断点。
func markLastUserTailForCache(messages []anthropicsdk.MessageParam) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != anthropicsdk.MessageParamRoleUser {
			continue
		}
		blocks := messages[i].Content
		if len(blocks) == 0 {
			return
		}
		last := &blocks[len(blocks)-1]
		switch {
		case last.OfText != nil:
			last.OfText.CacheControl = anthropicsdk.NewCacheControlEphemeralParam()
		case last.OfToolResult != nil:
			last.OfToolResult.CacheControl = anthropicsdk.NewCacheControlEphemeralParam()
		}
		return
	}
}

func buildAnthropicMessages(messages []conversation.Message) []anthropicsdk.MessageParam {
	var result []anthropicsdk.MessageParam
	for _, m := range messages {
		if m.Role == "assistant" {
			var blocks []anthropicsdk.ContentBlockParamUnion
			for _, tb := range m.ThinkingBlocks {
				blocks = append(blocks, anthropicsdk.NewThinkingBlock(tb.Signature, tb.Thinking))
			}
			if m.Content != "" {
				blocks = append(blocks, anthropicsdk.NewTextBlock(m.Content))
			}
			for _, tu := range m.ToolUses {
				blocks = append(blocks, anthropicsdk.ContentBlockParamUnion{
					OfToolUse: &anthropicsdk.ToolUseBlockParam{
						ID:    tu.ToolUseID,
						Name:  tu.ToolName,
						Input: tu.Arguments,
					},
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropicsdk.NewTextBlock(""))
			}
			result = append(result, anthropicsdk.MessageParam{
				Role:    anthropicsdk.MessageParamRoleAssistant,
				Content: blocks,
			})
		} else if len(m.ToolResults) > 0 {
			var blocks []anthropicsdk.ContentBlockParamUnion
			for _, tr := range m.ToolResults {
				// 带结构化 block 的走 block 数组（tool_reference 这类要求服务端
				// 解析的内容只能这么发），其余照旧发纯文本
				content := []anthropicsdk.ToolResultBlockParamContentUnion{{
					OfText: &anthropicsdk.TextBlockParam{Text: tr.Content},
				}}
				if structured := toolResultContentBlocks(tr.ContentBlocks); len(structured) > 0 {
					content = structured
				}
				blocks = append(blocks, anthropicsdk.ContentBlockParamUnion{
					OfToolResult: &anthropicsdk.ToolResultBlockParam{
						ToolUseID: tr.ToolUseID,
						IsError:   param.NewOpt(tr.IsError),
						Content:   content,
					},
				})
			}
			result = append(result, anthropicsdk.MessageParam{
				Role:    anthropicsdk.MessageParamRoleUser,
				Content: blocks,
			})
		} else {
			// 合并连续用户文本，维持 Anthropic 所需的角色交替。
			canMerge := false
			if n := len(result); n > 0 {
				prev := result[n-1]
				if prev.Role == anthropicsdk.MessageParamRoleUser && len(prev.Content) > 0 && prev.Content[0].OfToolResult == nil {
					canMerge = true
				}
			}
			if canMerge {
				result[len(result)-1].Content = append(result[len(result)-1].Content, anthropicsdk.NewTextBlock(m.Content))
			} else {
				result = append(result, anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(m.Content)))
			}
		}
	}
	return result
}

func classifyAnthropicError(err error) error {
	var apiErr *anthropicsdk.Error
	if errors.As(err, &apiErr) {
		retry := ""
		if apiErr.Response != nil {
			retry = apiErr.Response.Header.Get("Retry-After")
		}
		return llm.HTTPStatus(apiErr.StatusCode, apiErr.RawJSON(), err, retry)
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

// toolResultContentBlocks 把工具产出的结构化 block 转成 SDK 的类型化联合。
// 只认识 tool_reference——目前唯一需要服务端解析的块类型，官方端点下的
// ToolSearch 用它让服务端把 MCP 工具的 schema 展开进上下文。认不出的块整个
// 放弃转换，调用方会退回纯文本，宁可少发一个块也不发一个畸形请求。
func toolResultContentBlocks(raw []map[string]any) []anthropicsdk.ToolResultBlockParamContentUnion {
	if len(raw) == 0 {
		return nil
	}
	out := make([]anthropicsdk.ToolResultBlockParamContentUnion, 0, len(raw))
	for _, b := range raw {
		blockType, _ := b["type"].(string)
		if blockType != "tool_reference" {
			return nil
		}
		name, _ := b["tool_name"].(string)
		if name == "" {
			return nil
		}
		out = append(out, anthropicsdk.ToolResultBlockParamContentUnion{
			OfToolReference: &anthropicsdk.ToolReferenceBlockParam{ToolName: name},
		})
	}
	return out
}
