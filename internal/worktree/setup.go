package worktree

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// performPostCreationSetup 把设置、钩子、符号链接和被 Git 忽略的文件从主仓库传递到
// 新创建的工作树。
func performPostCreationSetup(ctx context.Context, repoRoot, worktreePath string) {
	// A. 复制 settings.local.json。
	copySettingsLocal(repoRoot, worktreePath)

	// B. 配置 Git 钩子路径。
	configureHooksPath(ctx, repoRoot, worktreePath)

	// C. 为大型目录创建符号链接（通过配置选择启用）。
	symlinkDirectories(repoRoot, worktreePath, getSymlinkDirectories())

	// D. 按 .worktreeinclude 复制被 Git 忽略的文件。
	CopyWorktreeIncludeFiles(ctx, repoRoot, worktreePath)
}

// copySettingsLocal 把 .ccode/settings.local.json 从主仓库复制到工作树，以传递可能包含
// 密钥的本地设置。
func copySettingsLocal(repoRoot, worktreePath string) {
	relPath := filepath.Join(".ccode", "settings.local.json")
	src := filepath.Join(repoRoot, relPath)
	dst := filepath.Join(worktreePath, relPath)

	srcData, err := os.ReadFile(src)
	if err != nil {
		return // ENOENT 属于正常情况——没有要复制的本地设置。
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, srcData, 0o644)
}

// configureHooksPath 在工作树中设置 core.hooksPath，以共享主仓库的 Git 钩子。
// .husky/ 的优先级高于 .git/hooks/。
func configureHooksPath(ctx context.Context, repoRoot, worktreePath string) {
	candidates := []string{
		filepath.Join(repoRoot, ".husky"),
		filepath.Join(repoRoot, ".git", "hooks"),
	}
	var hooksPath string
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err == nil && info.IsDir() {
			hooksPath = c
			break
		}
	}
	if hooksPath == "" {
		return
	}
	_, _, code := runGit(ctx, worktreePath, "config", "core.hooksPath", hooksPath)
	if code != 0 {
		// 尽力而为——不因钩子配置失败而让整个设置过程失败。
		return
	}
}

// symlinkDirectories 在 worktreePath 中创建指向 repoRoot 目录的符号链接，以避免磁盘
// 占用膨胀（例如 node_modules、vendor）。
func symlinkDirectories(repoRoot, worktreePath string, dirs []string) {
	for _, dir := range dirs {
		if strings.Contains(dir, "..") {
			continue // 路径遍历防护。
		}
		src := filepath.Join(repoRoot, dir)
		dst := filepath.Join(worktreePath, dir)
		// 符号链接采用尽力而为方式：源路径可能不存在，目标路径也可能已经存在。
		_ = os.Symlink(src, dst)
	}
}

// getSymlinkDirectories 返回配置为符号链接的目录列表。该列表通过
// settings.worktree.symlinkDirectories 配置；有配置时读取，默认值为空。
func getSymlinkDirectories() []string {
	return worktreeConfig.SymlinkDirectories
}

// CopyWorktreeIncludeFiles 把 .worktreeinclude 指定的、被 Git 忽略的文件从基础仓库
// 复制到工作树。匹配规则使用 gitignore 语法。
func CopyWorktreeIncludeFiles(ctx context.Context, repoRoot, worktreePath string) ([]string, error) {
	includeFile := filepath.Join(repoRoot, ".worktreeinclude")
	data, err := os.ReadFile(includeFile)
	if err != nil {
		return nil, nil // 没有 .worktreeinclude → 无需复制。
	}

	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if len(patterns) == 0 {
		return nil, nil
	}

	// 使用 git ls-files 列出被 Git 忽略的文件。
	stdout, _, code := runGit(ctx, repoRoot,
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	if code != 0 || strings.TrimSpace(stdout) == "" {
		return nil, nil
	}

	entries := strings.Split(strings.TrimSpace(stdout), "\n")

	// 简单模式匹配：对每个被 Git 忽略的文件，检查是否匹配任一 .worktreeinclude 模式。
	// 基本 glob 使用 filepath.Match，目录模式则使用前缀匹配。
	var toCopy []string
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		// 跳过折叠的目录（末尾带 /）。
		if strings.HasSuffix(entry, "/") {
			continue
		}
		if matchesWorktreeInclude(entry, patterns) {
			toCopy = append(toCopy, entry)
		}
	}

	var copied []string
	for _, rel := range toCopy {
		src := filepath.Join(repoRoot, rel)
		dst := filepath.Join(worktreePath, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := copyFileContents(src, dst); err != nil {
			continue
		}
		copied = append(copied, rel)
	}
	return copied, nil
}

// matchesWorktreeInclude 检查文件路径是否匹配任一 .worktreeinclude 模式。它支持
// 精确匹配、基本名匹配和基础 glob 模式。
func matchesWorktreeInclude(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, p := range patterns {
		p = strings.TrimPrefix(p, "/")
		// 精确匹配。
		if p == path || p == base {
			return true
		}
		// 对完整路径进行 glob 匹配。
		if matched, _ := filepath.Match(p, path); matched {
			return true
		}
		// 对基本名进行 glob 匹配。
		if matched, _ := filepath.Match(p, base); matched {
			return true
		}
		// 对目录模式进行前缀匹配。
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// WorktreeConfig 保存工作树相关配置，由 config.yaml 或默认值填充。
var worktreeConfig = struct {
	SymlinkDirectories   []string
	StaleCleanupInterval int // 单位为秒；0 表示禁用
	StaleCutoffHours     int // 单位为小时；默认 720（30 天）
}{
	StaleCutoffHours: 720,
}

// SetWorktreeConfig 允许 TUI/CLI 在启动时注入配置值。
func SetWorktreeConfig(symlinkDirs []string, cleanupIntervalSec, cutoffHours int) {
	worktreeConfig.SymlinkDirectories = symlinkDirs
	worktreeConfig.StaleCleanupInterval = cleanupIntervalSec
	if cutoffHours > 0 {
		worktreeConfig.StaleCutoffHours = cutoffHours
	}
}

// GetStaleCutoffHours 返回配置的截止时长，单位为小时。
func GetStaleCutoffHours() int {
	return worktreeConfig.StaleCutoffHours
}

// GetStaleCleanupInterval 返回配置的清理间隔，单位为秒。
func GetStaleCleanupInterval() int {
	return worktreeConfig.StaleCleanupInterval
}

// FindCanonicalGitRoot 穿透工作树解析主仓库根目录。从工作树内调用时，它会沿 .git
// 指针找到 commondir。
func FindCanonicalGitRoot(startDir string) string {
	gitDir, err := ResolveGitDir(startDir)
	if err != nil || gitDir == "" {
		return ""
	}
	// 如果 gitDir 包含 commondir 指针，则沿指针找到主仓库。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil || commonDir == "" {
		// gitDir 是主 .git 目录；其父目录就是仓库根目录。
		return filepath.Dir(gitDir)
	}
	// commonDir 指向主仓库的 .git；其父目录就是仓库根目录。
	return filepath.Dir(commonDir)
}
