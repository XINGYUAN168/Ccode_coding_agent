package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RelevantMemory 是选中并展示到主对话的一份记忆文件。MtimeMs 会一并传递，
// 让调用方无需再次 stat 就能呈现新鲜度。
type RelevantMemory struct {
	Path    string
	MtimeMs int64
}

// SelectorFn 抽象召回选择器使用的旁路 LLM 调用。调用方接收系统提示词和用户消息后，
// 应发起一次模型调用并返回原始助手文本。FindRelevantMemories 把错误视为“选择器失败，
// 不执行召回”。Ccode 的 llm.Client 是流式且绑定系统提示词的接口，因此该回调允许调用方
// 创建专用旁路查询客户端，而无需在包级建立 memory 到 llm 的耦合。
type SelectorFn func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// SelectMemoriesSystemPrompt 是选择器 Agent 使用的系统提示词。
const SelectMemoriesSystemPrompt = `You are selecting memories that will be useful to Ccode as it processes a user's query. You will be given the user's query and a list of available memory files with their filenames and descriptions.

Return a list of filenames for the memories that will clearly be useful to Ccode as it processes the user's query (up to 5). Only include memories that you are certain will be helpful based on their name and description.
- If you are unsure if a memory will be useful in processing the user's query, then do not include it in your list. Be selective and discerning.
- If there are no memories in the list that would clearly be useful, feel free to return an empty list.
- If a list of recently-used tools is provided, do not select memories that are usage reference or API documentation for those tools (Ccode is already exercising them). DO still select memories containing warnings, gotchas, or known issues about those tools — active use is exactly when those matter.

Respond with valid JSON only, no markdown, in this exact shape: {"selected_memories": ["filename1.md", "filename2.md"]}`

// FindRelevantMemories 扫描 userMemDir 和 projectMemDir，请选择器为 query 挑选最多 5 个
// 相关文件名，并返回对应绝对路径和修改时间。它排除已载入系统提示词的 MEMORY.md，
// 同时传回修改时间，让调用方无需再次 stat 即可展示新鲜度。
//
// alreadySurfaced 在调用选择器前过滤此前轮次已经展示的路径，使 5 个名额用于新候选，
// 而不是重复选择调用方随后会丢弃的文件。
//
// 任一目录都可以为空，此时只扫描非空目录。两个目录中的同名文件通过返回结果中的
// FilePath 区分。
//
// 选择器失败会被静默处理；召回只尽力执行，绝不能阻塞主对话。
// 选择或解析发生任何错误时返回空切片和 nil 错误。
func FindRelevantMemories(
	ctx context.Context,
	query string,
	userMemDir, projectMemDir string,
	recentTools []string,
	alreadySurfaced map[string]struct{},
	selector SelectorFn,
) ([]RelevantMemory, error) {
	if selector == nil {
		return nil, nil
	}
	var all []MemoryHeader
	if userMemDir != "" {
		userScan, err := ScanMemoryFiles(ctx, userMemDir, "user")
		if err != nil {
			return nil, err
		}
		all = append(all, userScan...)
	}
	if projectMemDir != "" {
		projectScan, err := ScanMemoryFiles(ctx, projectMemDir, "project")
		if err != nil {
			return nil, err
		}
		all = append(all, projectScan...)
	}
	memories := make([]MemoryHeader, 0, len(all))
	for _, m := range all {
		if _, ok := alreadySurfaced[m.FilePath]; ok {
			continue
		}
		memories = append(memories, m)
	}
	if len(memories) == 0 {
		return nil, nil
	}

	selectedFilenames, _ := selectRelevantMemories(ctx, query, memories, recentTools, selector)
	byKey := make(map[string]MemoryHeader, len(memories))
	for _, m := range memories {
		byKey[m.FilePath] = m
		// 同时按 Filename 建索引，以兼容选择器输出中省略路径的情况。
		if _, exists := byKey[m.Filename]; !exists {
			byKey[m.Filename] = m
		}
	}
	selected := make([]RelevantMemory, 0, len(selectedFilenames))
	for _, fn := range selectedFilenames {
		m, ok := byKey[fn]
		if !ok {
			continue
		}
		selected = append(selected, RelevantMemory{Path: m.FilePath, MtimeMs: m.MtimeMs})
	}
	return selected, nil
}

func selectRelevantMemories(
	ctx context.Context,
	query string,
	memories []MemoryHeader,
	recentTools []string,
	selector SelectorFn,
) ([]string, error) {
	validFilenames := make(map[string]struct{}, len(memories))
	for _, m := range memories {
		validFilenames[m.Filename] = struct{}{}
	}

	manifest := FormatMemoryManifest(memories)

	// Ccode 正在使用某工具（例如 mcp__X__spawn）时，再展示该工具的参考文档只会形成噪声，
	// 因为对话已经包含实际用法。否则选择器可能按关键词重合匹配，例如 query 和记忆说明中
	// 都有 "spawn" 时产生误报。
	toolsSection := ""
	if len(recentTools) > 0 {
		toolsSection = "\n\nRecently used tools: " + strings.Join(recentTools, ", ")
	}

	userMessage := fmt.Sprintf("Query: %s\n\nAvailable memories:\n%s%s", query, manifest, toolsSection)

	raw, err := selector(ctx, SelectMemoriesSystemPrompt, userMessage)
	if err != nil {
		return nil, nil
	}
	clean := extractJSONObject(raw)
	if clean == "" {
		return nil, nil
	}
	var parsed struct {
		SelectedMemories []string `json:"selected_memories"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(parsed.SelectedMemories))
	for _, f := range parsed.SelectedMemories {
		if _, ok := validFilenames[f]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// extractJSONObject 返回 raw 中找到的第一个 `{...}` 子串；若去除空白后已经以 `{` 开头，
// 则直接返回该文本。即使提示词有约束，它仍容忍 JSON 外包裹 Markdown 代码围栏或说明文字。
func extractJSONObject(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(trimmed, "}")
	if end < start {
		return ""
	}
	return trimmed[start : end+1]
}
