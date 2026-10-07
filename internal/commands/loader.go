package commands

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CommandMeta 是文件式提示命令的 frontmatter，保存旧式 /commands/ 文件使用的字段子集：
// description、argument-hint 和 aliases。
type CommandMeta struct {
	Description  string   `yaml:"description"`
	ArgumentHint string   `yaml:"argument-hint"`
	Aliases      []string `yaml:"aliases"`
}

// LoadDir 递归扫描 dir 中的 *.md 文件，每个文件返回一条 Command。命令名取自文件相对
// dir 的路径，并按原命名空间规则用 ':' 连接子目录（sub/dir/foo.md → "sub:dir:foo"）。
// 解析失败的文件会被静默跳过，格式错误的用户命令不应阻断启动。
//
// 每条返回命令的 Type 都是 TypePrompt，其 Handler 返回替换 $ARGUMENTS 后的 Markdown 正文。
// 若正文没有 $ARGUMENTS 占位符且 args 非空，则把参数追加到 "## User Request" 小节。
func LoadDir(dir string) []*Command {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	var cmds []*Command
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
			return nil
		}
		cmd := parseCommandFile(dir, path)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return nil
	})
	return cmds
}

// LoadUserCommands 合并两个搜索路径中的文件式命令：1. ~/.ccode/commands/（用户全局）；
// 2. $workDir/.ccode/commands/（项目级）。
//
// 名称冲突时，后加载的来源覆盖先加载的来源。
func LoadUserCommands(workDir string) []*Command {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".ccode", "commands"))
	}
	dirs = append(dirs,
		filepath.Join(workDir, ".ccode", "commands"),
	)

	merged := map[string]*Command{}
	var order []string
	for _, d := range dirs {
		for _, cmd := range LoadDir(d) {
			if _, seen := merged[cmd.Name]; !seen {
				order = append(order, cmd.Name)
			}
			merged[cmd.Name] = cmd
		}
	}

	out := make([]*Command, 0, len(order))
	for _, name := range order {
		out = append(out, merged[name])
	}
	return out
}

// parseCommandFile 读取单个 .md 文件并返回对应 Command；读取或解析失败时返回 nil。
// 名称根据相对路径计算：baseDir 下的 "git/log.md" → "git:log"。
// 名称会转为小写，以符合 Parse 中 /<name> 的查找约定。
func parseCommandFile(baseDir, path string) *Command {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return nil
	}
	rel = strings.TrimSuffix(rel, ".md")
	parts := strings.Split(rel, string(filepath.Separator))
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.ReplaceAll(p, " ", "-"))
	}
	name := strings.Join(parts, ":")
	if name == "" {
		return nil
	}

	meta, body := splitFrontmatter(string(data))
	body = strings.TrimSpace(body)
	if meta.Description == "" {
		meta.Description = firstNonHeaderLine(body)
	}

	return &Command{
		Name:        name,
		Description: meta.Description,
		Aliases:     meta.Aliases,
		Type:        TypePrompt,
		ArgPrompt:   meta.ArgumentHint,
		Handler:     promptHandler(body),
	}
}

// splitFrontmatter 把 YAML frontmatter 与 Markdown 正文分开。若没有 frontmatter 或解析失败，
// 则返回空 meta 和原始内容；格式错误的命令文件不应阻断启动。
func splitFrontmatter(content string) (CommandMeta, string) {
	var meta CommandMeta
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return meta, content
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return meta, content
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		return CommandMeta{}, content
	}
	return meta, parts[2]
}

// firstNonHeaderLine 返回第一条非空且不是标题的行，在 frontmatter 未提供说明时作为回退值。
func firstNonHeaderLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// promptHandler 返回一个 Handler，用 $ARGUMENTS 替换后渲染命令正文。
// 没有占位符的正文会把参数追加到 "## User Request" 小节。
func promptHandler(body string) Handler {
	return func(ctx *Context) string {
		if strings.Contains(body, "$ARGUMENTS") {
			return strings.ReplaceAll(body, "$ARGUMENTS", ctx.Args)
		}
		if strings.TrimSpace(ctx.Args) == "" {
			return body
		}
		return body + "\n\n## User Request\n\n" + ctx.Args
	}
}
