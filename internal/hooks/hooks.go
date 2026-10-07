package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// defaultHookTimeout 是 Hook 配置未设置自身 `timeout` 时采用的上限。
const defaultHookTimeout = 10 * time.Minute

type EventName string

const (
	EventSessionStart EventName = "session_start"
	EventSessionEnd   EventName = "session_end"
	EventTurnStart    EventName = "turn_start"
	EventTurnEnd      EventName = "turn_end"
	EventPreSend      EventName = "pre_send"
	EventPostReceive  EventName = "post_receive"
	EventPreToolUse   EventName = "pre_tool_use"
	EventPostToolUse  EventName = "post_tool_use"
	EventShutdown     EventName = "shutdown"
)

type ActionType string

const (
	ActionCommand ActionType = "command"
	ActionPrompt  ActionType = "prompt"
	ActionHTTP    ActionType = "http"
	ActionAgent   ActionType = "agent"
)

type Action struct {
	Type    ActionType        `yaml:"type"`
	Command string            `yaml:"command"`
	Message string            `yaml:"message"`
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Timeout time.Duration     `yaml:"timeout"`
}

type Hook struct {
	ID        string    `yaml:"id"`
	Event     EventName `yaml:"event"`
	Condition string    `yaml:"if"`
	Action    Action    `yaml:"action"`
	Reject    bool      `yaml:"reject"`
	Once      bool      `yaml:"once"`
	Async     bool      `yaml:"async"`
	// OnError 控制动作失败时的行为。
	//   "fail"   — 向上传播错误（阻塞式 Hook 的默认值）
	//   "ignore" — 记录日志并继续
	//   "reject" — 把 Hook 失败视为拒绝（仅用于 pre_tool_use）
	OnError string `yaml:"on_error"`
}

type HookContext struct {
	EventName EventName
	ToolName  string
	ToolArgs  map[string]any
	FilePath  string
	Message   string
	Error     string
}

type HookResult struct {
	HookID  string
	Output  string
	Success bool
	Reject  bool
}

type Engine struct {
	mu            sync.Mutex
	hooks         []Hook
	notifications []HookResult
	fired         map[string]bool // 已触发的 Hook ID（用于 `once`）
	// AgentRunner 执行 Agent 类型的 Hook。它是可选项；为 nil 时，Agent Hook 会返回明确的
	// "no runner registered" 错误，而不是静默失败。
	AgentRunner func(prompt string, ctx HookContext) (string, error)
}

func NewEngine() *Engine {
	return &Engine{fired: make(map[string]bool)}
}

// validEventNames 是 Validate 接受的事件名称白名单。它直接取自上方 EventName 常量，
// 因此新增事件只需修改一处。
var validEventNames = map[EventName]bool{
	EventSessionStart: true,
	EventSessionEnd:   true,
	EventTurnStart:    true,
	EventTurnEnd:      true,
	EventPreSend:      true,
	EventPostReceive:  true,
	EventPreToolUse:   true,
	EventPostToolUse:  true,
	EventShutdown:     true,
}

// Validate 检查一组 Hook 中会在运行时表现为静默异常的配置错误。每种动作类型都有自己的
// 必填字段；URL Hook 必须提供可解析的 http(s) URL；timeout 不能为负数。
//
// 所有错误通过 errors.Join 聚合，使一次调用就能展示全部问题，而不是遇到第一项就退出。
// 每条错误都以前缀标明 Hook ID（ID 为空时使用索引）和出错字段。
func Validate(hooks []Hook) error {
	var errs []error
	for i, h := range hooks {
		label := h.ID
		if label == "" {
			label = fmt.Sprintf("hook[%d]", i)
		} else {
			label = fmt.Sprintf("hook[%d] (id=%q)", i, h.ID)
		}

		if !validEventNames[h.Event] {
			errs = append(errs, fmt.Errorf("%s: unknown event %q", label, h.Event))
		}

		if h.Action.Timeout < 0 {
			errs = append(errs, fmt.Errorf("%s: action.timeout must be >= 0 (got %s)", label, h.Action.Timeout))
		}

		switch h.Action.Type {
		case ActionCommand:
			if strings.TrimSpace(h.Action.Command) == "" {
				errs = append(errs, fmt.Errorf("%s: action.command must be non-empty for type %q", label, h.Action.Type))
			}
		case ActionPrompt:
			if strings.TrimSpace(h.Action.Message) == "" {
				errs = append(errs, fmt.Errorf("%s: action.message must be non-empty for type %q", label, h.Action.Type))
			}
		case ActionHTTP:
			if strings.TrimSpace(h.Action.URL) == "" {
				errs = append(errs, fmt.Errorf("%s: action.url must be non-empty for type %q", label, h.Action.Type))
			} else {
				u, err := url.Parse(h.Action.URL)
				if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
					errs = append(errs, fmt.Errorf("%s: action.url must be a valid http(s) URL (got %q)", label, h.Action.URL))
				}
			}
		case ActionAgent:
			if strings.TrimSpace(h.Action.Message) == "" && strings.TrimSpace(h.Action.Command) == "" {
				errs = append(errs, fmt.Errorf("%s: action.message (or action.command as fallback) must be non-empty for type %q", label, h.Action.Type))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s: action.type is required", label))
		default:
			errs = append(errs, fmt.Errorf("%s: unknown action.type %q (want command / prompt / http / agent)", label, h.Action.Type))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) LoadHooks(hooks []Hook) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks = hooks
	e.fired = make(map[string]bool)
}

func (e *Engine) RunHooks(ctx HookContext) []HookResult {
	var results []HookResult
	for _, h := range e.snapshotHooks() {
		if h.Event != ctx.EventName {
			continue
		}
		if !e.shouldFire(h, ctx) {
			continue
		}
		if h.Async {
			go func(h Hook) {
				res := e.executeAction(h, ctx)
				e.recordNotification(res)
			}(h)
			results = append(results, HookResult{HookID: h.ID, Output: "(async)", Success: true})
			continue
		}
		result := e.executeAction(h, ctx)
		results = append(results, result)
		e.recordNotification(result)
	}
	return results
}

// RunPreToolHooks 运行工具调用前的 Hook，返回 (rejected, message)。
// 非拒绝型 Hook 仍会执行通知、HTTP 等副作用。
func (e *Engine) RunPreToolHooks(ctx HookContext) (bool, string) {
	for _, h := range e.snapshotHooks() {
		if h.Event != EventPreToolUse {
			continue
		}
		if !e.shouldFire(h, ctx) {
			continue
		}
		result := e.executeAction(h, ctx)
		e.recordNotification(result)
		// Hook 可以通过配置拒绝，也可以在失败且 on_error=reject 时拒绝。
		if h.Reject || (!result.Success && h.OnError == "reject") {
			msg := result.Output
			if msg == "" {
				msg = "blocked by hook " + h.ID
			}
			return true, msg
		}
	}
	return false, ""
}

func (e *Engine) shouldFire(h Hook, ctx HookContext) bool {
	if h.Condition != "" && !evaluateCondition(h.Condition, ctx) {
		return false
	}
	if h.Once {
		e.mu.Lock()
		defer e.mu.Unlock()
		if h.ID != "" && e.fired[h.ID] {
			return false
		}
		if h.ID != "" {
			e.fired[h.ID] = true
		}
	}
	return true
}

func (e *Engine) snapshotHooks() []Hook {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Hook, len(e.hooks))
	copy(out, e.hooks)
	return out
}

func (e *Engine) recordNotification(r HookResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifications = append(e.notifications, r)
}

func (e *Engine) DrainNotifications() []HookResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.notifications
	e.notifications = nil
	return n
}

// evaluateCondition 支持：
//   - leaf: `var == "value"`, `var =~ /regex/`, `var =* "glob"`, `var != "value"`
//   - 组合条件：`cond1 && cond2`、`cond1 || cond2`
//   - 取反条件：`!cond`（必须紧邻叶子条件，不能跨运算符拆分）
//
// 不支持括号；组合条件严格从左到右计算，`&&` 和 `||` 优先级相同，
// 与简单 shell 风格一致。
func evaluateCondition(condition string, ctx HookContext) bool {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return true
	}
	// 组合条件：遍历字符串，按顶层的 && 和 || 拆分。
	// Hook 条件由用户配置，因此不支持引号内包含这些运算符，保持解析规则简单明确。
	if tokens := splitComposite(condition); len(tokens) > 1 {
		result := evaluateCondition(tokens[0].expr, ctx)
		for i := 1; i < len(tokens); i++ {
			rhs := evaluateCondition(tokens[i].expr, ctx)
			if tokens[i].op == "&&" {
				result = result && rhs
			} else {
				result = result || rhs
			}
		}
		return result
	}

	if strings.HasPrefix(condition, "!") {
		return !evaluateCondition(strings.TrimSpace(condition[1:]), ctx)
	}

	return evaluateLeaf(condition, ctx)
}

type compToken struct {
	op   string // 首个 token 对应 ""，之后为 "&&" 或 "||"
	expr string
}

func splitComposite(s string) []compToken {
	var out []compToken
	start := 0
	cur := ""
	for i := 0; i < len(s)-1; i++ {
		pair := s[i : i+2]
		if pair == "&&" || pair == "||" {
			out = append(out, compToken{op: cur, expr: strings.TrimSpace(s[start:i])})
			cur = pair
			start = i + 2
			i++
		}
	}
	out = append(out, compToken{op: cur, expr: strings.TrimSpace(s[start:])})
	if len(out) == 1 {
		// 没有找到可拆分点。
		return nil
	}
	return out
}

func evaluateLeaf(condition string, ctx HookContext) bool {
	for _, op := range []string{"!=", "=~", "=*", "=="} {
		if idx := strings.Index(condition, op); idx >= 0 {
			left := strings.TrimSpace(condition[:idx])
			right := strings.Trim(strings.TrimSpace(condition[idx+len(op):]), `"'`)
			val := resolveVar(left, ctx)
			switch op {
			case "==":
				return val == right
			case "!=":
				return val != right
			case "=~":
				pattern := strings.Trim(right, "/")
				matched, _ := regexp.MatchString(pattern, val)
				return matched
			case "=*":
				matched, _ := filepath.Match(right, val)
				return matched
			}
		}
	}
	// 没有运算符 → 按变量真值检查处理。
	return resolveVar(condition, ctx) != ""
}

func resolveVar(name string, ctx HookContext) string {
	switch name {
	case "tool":
		return ctx.ToolName
	case "event":
		return string(ctx.EventName)
	case "file_path":
		return ctx.FilePath
	case "message":
		return ctx.Message
	}
	if strings.HasPrefix(name, "args.") {
		key := strings.TrimPrefix(name, "args.")
		if ctx.ToolArgs != nil {
			if v, ok := ctx.ToolArgs[key]; ok {
				return fmt.Sprintf("%v", v)
			}
		}
	}
	return ""
}

func (e *Engine) executeAction(h Hook, ctx HookContext) HookResult {
	switch h.Action.Type {
	case ActionCommand:
		return runCommand(h, ctx)
	case ActionPrompt:
		return HookResult{
			HookID:  h.ID,
			Output:  h.Action.Message,
			Success: true,
			Reject:  h.Reject,
		}
	case ActionHTTP:
		return runHTTP(h, ctx)
	case ActionAgent:
		return e.runAgent(h, ctx)
	default:
		return HookResult{
			HookID:  h.ID,
			Output:  fmt.Sprintf("Unknown action type: %s", h.Action.Type),
			Success: false,
		}
	}
}

// runAgent 使用 Hook 的 Message 作为一次性提示词调用可选的 AgentRunner。
// 未配置运行器时返回明确错误，提示用户需要在主入口接入 Agent 类型的 Hook。
func (e *Engine) runAgent(h Hook, ctx HookContext) HookResult {
	if e.AgentRunner == nil {
		return HookResult{
			HookID:  h.ID,
			Output:  "agent-type hook configured but no AgentRunner registered",
			Success: false,
			Reject:  h.Reject,
		}
	}
	prompt := h.Action.Message
	if prompt == "" {
		prompt = h.Action.Command
	}
	output, err := e.AgentRunner(prompt, ctx)
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	return HookResult{HookID: h.ID, Output: output, Success: true, Reject: h.Reject}
}

func runCommand(h Hook, ctx HookContext) HookResult {
	timeout := h.Action.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	execCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", h.Action.Command)
	cmd.Env = append(cmd.Environ(),
		"CCODE_EVENT="+string(ctx.EventName),
		"CCODE_TOOL="+ctx.ToolName,
		"CCODE_FILE_PATH="+ctx.FilePath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n" + stderr.String()
	}
	output = strings.TrimSpace(output)

	// 区分超时与普通执行失败，让用户和下游通知能够判断 Hook 退出原因。
	// CommandContext 在截止时间通过 SIGKILL 终止子进程，而 Run 本身只返回含义不明确的
	// *exec.ExitError，因此这里检查上下文。
	if execCtx.Err() == context.DeadlineExceeded {
		msg := fmt.Sprintf("command timed out after %s", timeout)
		if output != "" {
			msg = msg + ": " + output
		}
		return HookResult{
			HookID:  h.ID,
			Output:  msg,
			Success: false,
			Reject:  h.Reject,
		}
	}
	return HookResult{
		HookID:  h.ID,
		Output:  output,
		Success: err == nil,
		Reject:  h.Reject,
	}
}

func runHTTP(h Hook, ctx HookContext) HookResult {
	method := strings.ToUpper(h.Action.Method)
	if method == "" {
		method = "POST"
	}
	timeout := h.Action.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	body := h.Action.Body
	if body == "" {
		payload := map[string]any{
			"event":     string(ctx.EventName),
			"tool":      ctx.ToolName,
			"tool_args": ctx.ToolArgs,
			"file_path": ctx.FilePath,
			"message":   ctx.Message,
			"error":     ctx.Error,
		}
		if b, err := json.Marshal(payload); err == nil {
			body = string(b)
		}
	}

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, h.Action.URL, strings.NewReader(body))
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	for k, v := range h.Action.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" && body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	return HookResult{
		HookID:  h.ID,
		Output:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes))),
		Success: ok,
		Reject:  h.Reject,
	}
}
