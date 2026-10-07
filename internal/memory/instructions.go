package memory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxIncludeDepth 是 @include 指令允许的最大嵌套深度。
const MaxIncludeDepth = 5

// InstructionSource 表示一份已加载的指令文件。
type InstructionSource struct {
	Path    string
	Content string
}

// LoadInstructions 发现并拼接项目级与用户级指令文件。
//
// 发现顺序如下；越靠后的层越晚追加，因此模型会优先关注：
//  1. 用户全局：~/.ccode/CCODE.md、~/.ccode/AGENTS.md
//  2. 项目级：从 Git 根目录向下遍历到 workDir，收集每个目录中的 CCODE.md、
//     AGENTS.md 和 .ccode/CCODE.md，使最接近 cwd 的文件优先级最高
//  3. workDir/CCODE.local.md（私有本地覆盖）
//
// @include 指令：
//   - 支持 @./relative/path、@~/home/path 或 @/absolute/path
//   - 相对于包含该指令的文件目录解析
//   - 位于围栏代码块内时跳过
//   - 防止循环，同一绝对路径不会重复包含
func LoadInstructions(workDir string) string {
	sources := DiscoverInstructions(workDir)
	if len(sources) == 0 {
		return ""
	}
	var parts []string
	for _, s := range sources {
		label := s.Path
		if rel, err := filepath.Rel(workDir, s.Path); err == nil && !strings.HasPrefix(rel, "..") {
			label = rel
		}
		parts = append(parts, fmt.Sprintf("Contents of %s:\n\n%s", label, strings.TrimRight(s.Content, "\n")))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// DiscoverInstructions 按优先级返回已加载的源文件，低优先级在前。
// LoadInstructions 使用此函数，同时将其公开给测试。
func DiscoverInstructions(workDir string) []InstructionSource {
	var sources []InstructionSource
	seen := map[string]bool{}

	// 确定项目根目录，用于 @include 路径边界检查
	absWorkDir, _ := filepath.Abs(workDir)
	projectRoot := findGitRoot(absWorkDir)
	if projectRoot == "" {
		projectRoot = absWorkDir
	}

	if home, err := os.UserHomeDir(); err == nil {
		add(&sources, seen, filepath.Join(home, ".ccode", "CCODE.md"), projectRoot)
		add(&sources, seen, filepath.Join(home, ".ccode", "AGENTS.md"), projectRoot)
	}
	for _, dir := range projectInstructionDirs(workDir) {
		add(&sources, seen, filepath.Join(dir, "CCODE.md"), projectRoot)
		add(&sources, seen, filepath.Join(dir, "AGENTS.md"), projectRoot)
		// .ccode/ 下的同名文件：想让指令进 .gitignore 的项目放这里
		add(&sources, seen, filepath.Join(dir, ".ccode", "CCODE.md"), projectRoot)
	}
	add(&sources, seen, filepath.Join(workDir, "CCODE.local.md"), projectRoot)
	return sources
}

func add(out *[]InstructionSource, seen map[string]bool, path, projectRoot string) {
	abs, err := filepath.Abs(path)
	if err != nil || seen[abs] {
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	seen[abs] = true
	content := expandIncludes(string(data), filepath.Dir(abs), projectRoot, seen, 0)
	*out = append(*out, InstructionSource{Path: abs, Content: content})
}

// expandIncludes 展开 @include 指令。projectRoot 用于边界检查，
// 防止通过 ../ 逃逸到项目目录之外的任意位置。
func expandIncludes(content, baseDir, projectRoot string, seen map[string]bool, depth int) string {
	if depth > MaxIncludeDepth {
		return content
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out strings.Builder
	inCode := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		if !inCode {
			if path := parseInclude(trimmed); path != "" {
				resolved := resolveInclude(path, baseDir)
				if abs, err := filepath.Abs(resolved); err == nil && !seen[abs] {
					// @include 路径边界检查：不允许逃逸到项目目录和用户 home 之外
					if !isIncludeAllowed(abs, projectRoot) {
						out.WriteString("<!-- @include skipped: path outside project -->\n")
						continue
					}
					if data, err := os.ReadFile(abs); err == nil {
						seen[abs] = true
						out.WriteString(fmt.Sprintf("<!-- included from %s -->\n", path))
						out.WriteString(expandIncludes(string(data), filepath.Dir(abs), projectRoot, seen, depth+1))
						out.WriteByte('\n')
						continue
					}
				}
				// include 无法解析或读取时继续走普通路径，输出原始行以便用户注意。
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// isIncludeAllowed 检查 @include 解析后的绝对路径是否在允许范围内。
// 允许范围：项目目录（projectRoot）及其子目录，以及用户 home 目录下的 .ccode/。
// 这可以防止通过 @../../etc/passwd 等路径逃逸到任意位置。
func isIncludeAllowed(absPath, projectRoot string) bool {
	// 项目目录内的路径始终允许
	if projectRoot != "" && strings.HasPrefix(absPath, projectRoot+string(filepath.Separator)) {
		return true
	}
	if absPath == projectRoot {
		return true
	}
	// 用户 home 下的 .ccode/ 目录也允许（全局指令文件的 include）
	if home, err := os.UserHomeDir(); err == nil {
		ccodeDir := filepath.Join(home, ".ccode")
		if strings.HasPrefix(absPath, ccodeDir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// parseInclude 从 "@./path"、"@~/path" 或 "@/abs/path" 形式的行返回包含路径，
// 其它情况返回 ""。为避免误报，其它 @ token（例如 @username）会被忽略。
func parseInclude(trimmed string) string {
	if !strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "@@") {
		return ""
	}
	rest := strings.TrimPrefix(trimmed, "@")
	if rest == "" {
		return ""
	}
	if strings.ContainsAny(rest, " \t") {
		return ""
	}
	switch {
	case strings.HasPrefix(rest, "./"), strings.HasPrefix(rest, "../"),
		strings.HasPrefix(rest, "~/"), strings.HasPrefix(rest, "/"):
		return rest
	}
	return ""
}

func resolveInclude(p, baseDir string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// projectInstructionDirs 返回从 Git 根目录向下到 workDir 的目录列表。
// 若 workDir 不在 Git 仓库中，则只返回 [workDir]。
func projectInstructionDirs(workDir string) []string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return []string{workDir}
	}
	root := findGitRoot(abs)
	if root == "" {
		return []string{abs}
	}
	var dirs []string
	cur := abs
	for {
		dirs = append([]string{cur}, dirs...)
		if cur == root {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return dirs
}

func findGitRoot(start string) string {
	cur := start
	for {
		if info, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			_ = info
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
