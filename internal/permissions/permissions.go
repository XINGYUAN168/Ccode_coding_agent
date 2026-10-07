package permissions

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"ccode/internal/tools"
)

// splitCompoundCommand 拆分 shell 复合命令（&&、||、;、|）为独立子命令，
// 逐条匹配权限规则，防止通过 "cmd1 && dangerous_cmd" 绕过检查。
func splitCompoundCommand(cmd string) []string {
	parts := regexp.MustCompile(`\s*(?:&&|\|\||[;|])\s*`).Split(cmd, -1)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return []string{cmd}
	}
	return result
}

type DecisionEffect string

const (
	Allow DecisionEffect = "allow"
	Deny  DecisionEffect = "deny"
	Ask   DecisionEffect = "ask"
)

type Decision struct {
	Effect DecisionEffect
	Reason string
}

type PermissionMode string

const (
	ModeDefault     PermissionMode = "default"
	ModeAcceptEdits PermissionMode = "acceptEdits"
	ModePlan        PermissionMode = "plan"
	ModeBypass      PermissionMode = "bypassPermissions"
)

var modeMatrix = map[PermissionMode]map[tools.ToolCategory]DecisionEffect{
	ModeDefault:     {tools.CategoryRead: Allow, tools.CategoryWrite: Ask, tools.CategoryCommand: Ask},
	ModeAcceptEdits: {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Ask},
	ModeBypass:      {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Allow},
}

func ModeDecide(mode PermissionMode, category tools.ToolCategory) DecisionEffect {
	m, ok := modeMatrix[mode]
	if !ok {
		return Ask
	}
	return m[category]
}

// 第 1 层：危险命令检测。

type dangerousPattern struct {
	re     *regexp.Regexp
	reason string
}

var defaultDangerousPatterns = []dangerousPattern{
	{regexp.MustCompile(`rm\s+-[a-z]*r[a-z]*f[a-z]*\s+/\s*$`), "recursive force delete root"},
	{regexp.MustCompile(`mkfs\.`), "format disk"},
	{regexp.MustCompile(`dd\s+if=.*of=/dev/`), "direct write to disk device"},
	{regexp.MustCompile(`chmod\s+-R\s+777\s+/`), "recursive chmod root"},
	{regexp.MustCompile(`:\(\)\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`curl\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`wget\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`>\s*/dev/sd`), "overwrite disk device"},
	// Git 破坏性命令——防止误操作丢失工作
	{regexp.MustCompile(`git\s+push\s+.*--force`), "force push"},
	{regexp.MustCompile(`git\s+reset\s+--hard`), "hard reset"},
	{regexp.MustCompile(`git\s+clean\s+-f`), "force clean untracked files"},
	{regexp.MustCompile(`git\s+checkout\s+\.`), "discard all changes"},
	{regexp.MustCompile(`git\s+branch\s+-D`), "force delete branch"},
}

func DetectDangerous(command string) (bool, string) {
	for _, p := range defaultDangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}

// 第 2 层：路径沙箱。

type PathSandbox struct {
	allowedRoots []string
	denyWrite    []string // 始终只读的受保护路径，优先级高于 allowedRoots
}

// NewPathSandbox 创建路径沙箱。denyWrite 指定受保护路径（如配置文件），
// 即使在 allowedRoots 内也拒绝写入。
func NewPathSandbox(projectRoot string, extraAllowed ...string) *PathSandbox {
	root, _ := filepath.Abs(projectRoot)
	allowed := []string{root, os.TempDir()}
	for _, p := range extraAllowed {
		abs, _ := filepath.Abs(p)
		allowed = append(allowed, abs)
	}

	// 默认受保护路径：防止 Agent 篡改权限配置和 Skill 定义
	denyWrite := []string{
		filepath.Join(root, ".ccode", "config.yaml"),
		filepath.Join(root, ".ccode", "permissions.local.yaml"),
		filepath.Join(root, ".ccode", "skills"),
	}

	return &PathSandbox{allowedRoots: allowed, denyWrite: denyWrite}
}

// CheckDenyWrite 单独检查受保护路径。这类路径存放权限配置与 Skill 定义，
// 任何权限模式下都不允许写入，调用方需要在模式判断之前调用它。
func (s *PathSandbox) CheckDenyWrite(path string) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path: %s", path)
	}
	for _, deny := range s.denyWrite {
		if abs == deny || strings.HasPrefix(abs, deny+string(filepath.Separator)) {
			return false, fmt.Sprintf("protected path: %s", path)
		}
	}
	return true, ""
}

func (s *PathSandbox) Check(path string) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path: %s", path)
	}

	if ok, reason := s.CheckDenyWrite(path); !ok {
		return false, reason
	}

	for _, root := range s.allowedRoots {
		if strings.HasPrefix(abs, root) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("path %s outside sandbox", path)
}

// GetDenyWrite 返回受保护路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetDenyWrite() []string {
	return s.denyWrite
}

// GetAllowedRoots 返回允许写入的路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetAllowedRoots() []string {
	return s.allowedRoots
}

// 第 3 层：规则引擎。

type RuleEffect string

const (
	RuleAllow RuleEffect = "allow"
	RuleDeny  RuleEffect = "deny"
	RuleAsk   RuleEffect = "ask"
)

type Rule struct {
	ToolName string
	Pattern  string
	Effect   RuleEffect
}

func (r Rule) Matches(toolName, content string) bool {
	if r.ToolName != toolName {
		return false
	}
	// 简单通配符匹配：* 匹配任意字符（包括 /），适用于 Bash 命令等非路径场景。
	// filepath.Match 的 * 不匹配 /，导致 "allow always" 对含路径的命令失效。
	return globMatch(r.Pattern, content)
}

// globMatch 实现简单的通配符匹配，* 匹配任意字符（包括 /）。
func globMatch(pattern, content string) bool {
	// 快捷路径：无通配符时做精确比较
	if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
		return pattern == content
	}
	// 将 pattern 转为正则：* → .*, ? → .
	re := "^"
	for _, ch := range pattern {
		switch ch {
		case '*':
			re += ".*"
		case '?':
			re += "."
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			re += "\\" + string(ch)
		default:
			re += string(ch)
		}
	}
	re += "$"
	matched, _ := regexp.MatchString(re, content)
	return matched
}

// cachedRules 是单个规则文件的解析结果。modTime + size 一起作为文件是否变动的依据，
// 只比 modTime 不够：同一秒内的连续改写在部分文件系统上时间戳可能不变。
type cachedRules struct {
	modTime time.Time
	size    int64
	rules   []Rule
}

type RuleEngine struct {
	UserPath    string
	ProjectPath string
	LocalPath   string

	// 后台记忆 Agent 与主 Agent 可能共用同一个引擎，缓存读写要加锁
	mu    sync.Mutex
	cache map[string]cachedRules
}

// NewRuleEngine 按约定路径构造规则引擎：用户级放在 home 目录下，
// 项目级与本地级放在工作目录下。取不到 home 时用户级留空，该层按空规则处理。
func NewRuleEngine(workDir string) *RuleEngine {
	e := &RuleEngine{
		ProjectPath: filepath.Join(workDir, ".ccode", "permissions.yaml"),
		LocalPath:   filepath.Join(workDir, ".ccode", "permissions.local.yaml"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		e.UserPath = filepath.Join(home, ".ccode", "permissions.yaml")
	}
	return e
}

// Evaluate 把三份规则文件的规则合并成一个集合，返回命中规则中最严格的效果。
// 优先级 deny > ask > allow：规则写在哪一层、写在文件第几行都不影响裁决，
// 因此一条 deny 无法被其他层的 allow 抵消。没有任何规则命中时返回 nil。
func (e *RuleEngine) Evaluate(toolName, content string) *RuleEffect {
	return EvaluateRules(e.Snapshot(), toolName, content)
}

// Snapshot 取三份规则文件的合并快照。文件没变动时直接复用上次的解析结果，
// 变动了才重新读盘，因此改完规则文件下次评估即刻生效，反复评估也不会重复解析。
// 一次工具调用取一次快照，复合命令逐条检查子命令时共用它。
func (e *RuleEngine) Snapshot() []Rule {
	var all []Rule
	for _, path := range []string{e.UserPath, e.ProjectPath, e.LocalPath} {
		all = append(all, e.rulesFor(path)...)
	}
	return all
}

// rulesFor 返回单个规则文件的规则，命中缓存时不读盘也不解析。
func (e *RuleEngine) rulesFor(path string) []Rule {
	if path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		// 文件不存在或读不到，按空规则处理，同时清掉可能存在的旧缓存
		e.mu.Lock()
		delete(e.cache, path)
		e.mu.Unlock()
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.cache[path]; ok && c.modTime.Equal(info.ModTime()) && c.size == info.Size() {
		return c.rules
	}

	rules := loadRulesFile(path)
	if e.cache == nil {
		e.cache = make(map[string]cachedRules)
	}
	e.cache[path] = cachedRules{modTime: info.ModTime(), size: info.Size(), rules: rules}
	return rules
}

// EvaluateRules 在给定规则集上裁决，优先级 deny > ask > allow。
// 没有任何规则命中时返回 nil。
func EvaluateRules(rules []Rule, toolName, content string) *RuleEffect {
	var hit *RuleEffect
	for _, r := range rules {
		if !r.Matches(toolName, content) {
			continue
		}
		switch r.Effect {
		case RuleDeny:
			// deny 已是最严效果，不可能再被压过，直接返回
			eff := RuleDeny
			return &eff
		case RuleAsk:
			eff := RuleAsk
			hit = &eff
		case RuleAllow:
			// allow 最弱，只在还没命中更严的效果时记录
			if hit == nil {
				eff := RuleAllow
				hit = &eff
			}
		}
	}
	return hit
}

func (e *RuleEngine) AppendLocalRule(r Rule) {
	if e.LocalPath == "" {
		return
	}
	os.MkdirAll(filepath.Dir(e.LocalPath), 0o755)
	rules := loadRulesFile(e.LocalPath)
	rules = append(rules, r)
	var entries []map[string]string
	for _, rule := range rules {
		entries = append(entries, map[string]string{
			"rule":   fmt.Sprintf("%s(%s)", rule.ToolName, rule.Pattern),
			"effect": string(rule.Effect),
		})
	}
	data, _ := yaml.Marshal(entries)
	os.WriteFile(e.LocalPath, data, 0o644)
}

func loadRulesFile(path string) []Rule {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []struct {
		RuleStr string `yaml:"rule"`
		Effect  string `yaml:"effect"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var rules []Rule
	for _, e := range entries {
		if e.Effect != "allow" && e.Effect != "deny" && e.Effect != "ask" {
			continue
		}
		r, err := parseRule(e.RuleStr, RuleEffect(e.Effect))
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

var ruleRE = regexp.MustCompile(`^(\w+)\((.+)\)$`)

func parseRule(raw string, effect RuleEffect) (Rule, error) {
	m := ruleRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Rule{}, fmt.Errorf("invalid rule syntax: %s", raw)
	}
	return Rule{ToolName: m[1], Pattern: m[2], Effect: effect}, nil
}

// 提取用于规则匹配的内容。

var contentFields = map[string]string{
	"Bash": "command", "ReadFile": "file_path", "WriteFile": "file_path",
	"EditFile": "file_path", "Glob": "pattern", "Grep": "pattern",
}

func ExtractContent(toolName string, args map[string]any) string {
	// mcp_call 的匹配对象不是某一个参数，而是「要调用哪个 MCP 工具」，由
	// server + tool 两个参数合成 server__tool。这样规则写成 mcp_call(linear__*)
	// 就能按服务器或按工具做 allow/deny。
	if toolName == tools.McpCallToolName {
		server, _ := args["server"].(string)
		tool, _ := args["tool"].(string)
		return tools.McpCallPermissionContent(server, tool)
	}
	field, ok := contentFields[toolName]
	if !ok {
		return ""
	}
	v, _ := args[field].(string)
	return v
}

// DescribeToolAction 为 HITL 确认生成人类可读的操作描述。
// 优先从标准内容字段（command, file_path 等）提取；无法提取时拼接参数摘要。
func DescribeToolAction(toolName string, args map[string]any) string {
	content := ExtractContent(toolName, args)
	if content != "" {
		return content
	}
	// 无标准字段时，拼接参数的简短摘要
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	return toolName
}

// 第 4、5 层：权限检查器，负责编排所有层。

type Checker struct {
	Sandbox      *PathSandbox
	RuleEngine   *RuleEngine
	Mode         PermissionMode
	PlanFilePath string
	// SandboxEnabled 表示是否启用 OS 级沙箱。
	// 启用后 Bash 命令在沙箱内执行，配合 autoAllow 可跳过确认。
	SandboxEnabled bool
}

func NewChecker(sandbox *PathSandbox, ruleEngine *RuleEngine, mode PermissionMode) *Checker {
	return &Checker{
		Sandbox:    sandbox,
		RuleEngine: ruleEngine,
		Mode:       mode,
	}
}

func (c *Checker) Check(tool tools.Tool, args map[string]any) Decision {
	content := ExtractContent(tool.Name(), args)
	cat := tool.Category()

	// 规则快照按需取一次：安全命令、危险命令这些在前面几层就返回，压根不必碰规则文件；
	// 复合命令逐条检查子命令时共用同一份快照，不重复读盘
	snapshot := sync.OnceValue(c.RuleEngine.Snapshot)

	// 第 0 层：Plan 模式下写入计划文件的例外。
	if c.Mode == ModePlan && cat == tools.CategoryWrite && isPlanFile(content, c.PlanFilePath) {
		return Decision{Effect: Allow, Reason: "Plan mode: plan file write allowed"}
	}

	// 第 1 层：安全的只读命令，自动允许。
	if cat == tools.CategoryCommand && tools.IsSafeCommand(content) {
		return Decision{Effect: Allow, Reason: "Safe read-only command"}
	}

	// 第 2 层：危险命令，仅适用于 Bash。
	// 黑名单是硬防线，无论沙箱是否开启都必须先过一遍
	if cat == tools.CategoryCommand {
		hit, reason := DetectDangerous(content)
		if hit {
			return Decision{Effect: Deny, Reason: fmt.Sprintf("Dangerous command blocked: %s", reason)}
		}
	}

	// Layer 2b: 沙箱自动放行 — 命令在 OS 沙箱内执行时无需确认，
	// 但显式 deny/ask 规则仍然生效。
	// 拆分复合命令逐条检查，任何子命令触发 deny 则整体 deny，触发 ask 则弹窗。
	if c.SandboxEnabled && cat == tools.CategoryCommand {
		subcommands := splitCompoundCommand(content)
		var hasAsk bool
		for _, sub := range subcommands {
			r := EvaluateRules(snapshot(), tool.Name(), sub)
			if r != nil && *r == RuleDeny {
				return Decision{Effect: Deny, Reason: "Permission rule: deny"}
			}
			if r != nil && *r == RuleAsk {
				hasAsk = true
			}
		}
		if hasAsk {
			return Decision{Effect: Ask, Reason: "Permission rule: ask (sandbox does not override explicit ask)"}
		}
		return Decision{Effect: Allow, Reason: "Sandboxed: auto-allow"}
	}

	// 第 3 层：文件工具的路径沙箱。
	if (cat == tools.CategoryRead || cat == tools.CategoryWrite) && content != "" {
		// 受保护路径优先判定：写入权限配置或 Skill 定义一律拒绝，bypass 模式同样拦截
		if cat == tools.CategoryWrite {
			if ok, reason := c.Sandbox.CheckDenyWrite(content); !ok {
				return Decision{Effect: Deny, Reason: reason}
			}
		}
		ok, reason := c.Sandbox.Check(content)
		if !ok {
			if c.Mode == ModeBypass {
				// bypass 模式跳过沙箱确认，直接放行
			} else {
				return Decision{Effect: Ask, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
			}
		}
	}

	// 第 4 层：规则引擎。
	ruleResult := EvaluateRules(snapshot(), tool.Name(), content)
	if ruleResult != nil {
		switch *ruleResult {
		case RuleAllow:
			return Decision{Effect: Allow, Reason: "Permission rule: allow"}
		case RuleAsk:
			return Decision{Effect: Ask, Reason: "Permission rule: ask"}
		default:
			return Decision{Effect: Deny, Reason: "Permission rule: deny"}
		}
	}

	// 第 4 层：权限模式。
	effect := ModeDecide(c.Mode, cat)
	if effect == Allow {
		return Decision{Effect: Allow, Reason: fmt.Sprintf("Permission mode %s: allow", c.Mode)}
	}
	if effect == Deny {
		return Decision{Effect: Deny, Reason: fmt.Sprintf("Permission mode %s: deny", c.Mode)}
	}

	// 第 5 层：ASK → 人工介入。
	return Decision{Effect: Ask, Reason: "User confirmation required"}
}

func isPlanFile(targetPath, planPath string) bool {
	if planPath == "" || targetPath == "" {
		return false
	}
	// 先比较绝对路径。
	absTarget, err1 := filepath.Abs(targetPath)
	absPlan, err2 := filepath.Abs(planPath)
	if err1 == nil && err2 == nil && absTarget == absPlan {
		return true
	}
	// 检查目标是否以计划文件的相对路径后缀结尾。
	cleanTarget := filepath.Clean(targetPath)
	cleanPlan := filepath.Clean(planPath)
	if cleanTarget == cleanPlan {
		return true
	}
	// 比较基本名称：LLM 偶尔会把 file_path 缩短为基本名称。计划 slug 由
	// 形容词、名词和时间戳随机生成，因此几乎不会与同名无关文件冲突。
	if filepath.Base(cleanTarget) == filepath.Base(cleanPlan) {
		return true
	}
	return false
}

// IsSafeCommand 判断一条命令是不是只读的安全命令。
//
// 实现搬到了 tools 包：并发调度也要用同一份判定（只读命令可以跟只读工具一起跑），
// 而 tools 包不能反过来依赖 permissions。这里保留一层转发，规则层的调用点不用改。
func IsSafeCommand(command string) bool { return tools.IsSafeCommand(command) }
