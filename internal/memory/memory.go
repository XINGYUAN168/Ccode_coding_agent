package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Manager 封装用户级和项目级两个自动记忆目录。它只负责协调，实际保存和加载由
// Agent 的 Write/Read 工具完成；该结构为 TUI 构建系统提示词和处理 /memory
// 列表、清理命令提供稳定入口。
type Manager struct {
	projectRoot string
	userMemDir  string // ~/.ccode/memory/：user/feedback 类型记忆
	memDir      string // <projectRoot>/.ccode/memory/：project/reference 类型记忆
}

// NewManager 为给定项目根目录创建 Manager，并解析用户级和项目级记忆目录。
// 任一目录都可能为空，例如无法解析用户目录时用户级目录为空。
func NewManager(projectRoot string) *Manager {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return &Manager{
		projectRoot: abs,
		userMemDir:  GetUserAutoMemPath(),
		memDir:      GetAutoMemPath(abs),
	}
}

// Dir 返回末尾带分隔符的项目级记忆目录，供只关心项目作用域状态的调用方使用。
func (m *Manager) Dir() string {
	return m.memDir
}

// UserDir 返回末尾带分隔符的用户级记忆目录。
func (m *Manager) UserDir() string {
	return m.userMemDir
}

// EntrypointPath 返回项目级 MEMORY.md 的绝对路径。
func (m *Manager) EntrypointPath() string {
	return filepath.Join(m.memDir, AutoMemEntrypointName)
}

// UserEntrypointPath 返回用户级 MEMORY.md 的绝对路径。
func (m *Manager) UserEntrypointPath() string {
	if m.userMemDir == "" {
		return ""
	}
	return filepath.Join(m.userMemDir, AutoMemEntrypointName)
}

// BuildSystemReminder 返回可直接写入系统提示词的 `# auto memory` 小节。
// 它确保两个目录都存在，使 Agent 无需先运行 mkdir 即可写入任一目录。
func (m *Manager) BuildSystemReminder() string {
	if m.memDir == "" && m.userMemDir == "" {
		return ""
	}
	if m.userMemDir != "" {
		_ = EnsureMemoryDirExists(m.userMemDir)
	}
	if m.memDir != "" {
		_ = EnsureMemoryDirExists(m.memDir)
	}
	return BuildMemoryPrompt(autoMemDisplayName, m.userMemDir, m.memDir)
}

// MemoryFile 描述一份已保存的记忆。
type MemoryFile struct {
	Path        string
	Name        string
	Description string
	Type        MemoryType
}

// GetMemories 返回记忆目录中每个记忆文件的单行摘要，供 `/memory list` 斜杠命令使用。
// 返回顺序稳定，按文件名排序。
func (m *Manager) GetMemories() []string {
	files := m.LoadAll()
	out := make([]string, 0, len(files))
	for _, f := range files {
		typeTag := string(f.Type)
		if typeTag == "" {
			typeTag = "?"
		}
		desc := f.Description
		if desc == "" {
			desc = filepath.Base(f.Path)
		}
		out = append(out, fmt.Sprintf("[%s] %s — %s", typeTag, f.Name, desc))
	}
	return out
}

// LoadAll 扫描用户级和项目级记忆目录中的 *.md 文件，但排除 MEMORY.md，
// 并返回各文件解析后的 frontmatter。用户级文件在前，项目级文件在后。
func (m *Manager) LoadAll() []MemoryFile {
	out := loadDir(m.userMemDir)
	out = append(out, loadDir(m.memDir)...)
	return out
}

func loadDir(dir string) []MemoryFile {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var out []MemoryFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == AutoMemEntrypointName || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		mf := parseFrontmatter(string(data))
		mf.Path = path
		if mf.Name == "" {
			mf.Name = strings.TrimSuffix(name, ".md")
		}
		out = append(out, mf)
	}
	return out
}

// Clear 删除两个记忆目录中的所有 *.md 文件，包括 MEMORY.md，供 `/memory clear` 斜杠命令使用。
func (m *Manager) Clear() {
	clearDir(m.userMemDir)
	clearDir(m.memDir)
}

func clearDir(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

var frontmatterRe = regexp.MustCompile(`(?s)\A---\s*\n(.*?)\n---\s*\n`)

// parseFrontmatter 从类似 YAML 的 frontmatter 中提取 name、description、type。
// 只读取这三个已知字段，其它内容会被忽略，包括未知字段和引号等完整 YAML 语义。
// 没有 frontmatter 的文件会安全降级：字段为空，整个文件作为正文。
func parseFrontmatter(content string) MemoryFile {
	var mf MemoryFile
	m := frontmatterRe.FindStringSubmatch(content)
	if m == nil {
		return mf
	}
	for _, line := range strings.Split(m[1], "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			mf.Name = val
		case "description":
			mf.Description = val
		case "type":
			if t, ok := ParseMemoryType(val); ok {
				mf.Type = t
			}
		}
	}
	return mf
}
