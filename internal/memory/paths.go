package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// AutoMemEntrypointName 是每项目记忆索引的文件名。
const AutoMemEntrypointName = "MEMORY.md"

// GetAutoMemPath 返回给定项目根目录的自动记忆目录路径，形式为
// <projectRoot>/.ccode/memory/。
//
// 保留末尾分隔符，使基于前缀的路径匹配（例如沙箱 `HasPrefix` 检查）能够正确工作，
// 同时避免错误匹配
// `…/memoryxyz`.
//
// Ccode 把记忆与其它项目本地状态一起放在 .ccode/ 下，使记录可在 IDE 中显示，
// 也能由编辑器直接打开。
//
// 解析顺序：
//  1. CCODE_REMOTE_MEMORY_DIR 环境变量：原样使用，供 CI/容器等需要把记忆放在其它位置的场景使用
//  2. 默认路径：<projectRoot>/.ccode/memory
func GetAutoMemPath(projectRoot string) string {
	if override := os.Getenv("CCODE_REMOTE_MEMORY_DIR"); override != "" {
		return strings.TrimRight(override, string(filepath.Separator)) + string(filepath.Separator)
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return filepath.Join(abs, ".ccode", "memory") + string(filepath.Separator)
}

// GetAutoMemEntrypoint 返回自动记忆目录中 MEMORY.md 的路径。
func GetAutoMemEntrypoint(projectRoot string) string {
	dir := GetAutoMemPath(projectRoot)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsAutoMemPath 检查绝对路径是否位于项目级或用户级自动记忆目录中。
// 路径沙箱用它允许写入任一记忆目录。
func IsAutoMemPath(absolutePath, projectRoot string) bool {
	abs := filepath.Clean(absolutePath)
	if dir := GetAutoMemPath(projectRoot); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	if dir := GetUserAutoMemPath(); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	return false
}

// GetUserAutoMemPath 返回用户级自动记忆目录 ~/.ccode/memory/，用于跨项目跟随用户的
// type=user 和 type=feedback 记忆，例如编码偏好。无法解析用户目录时返回 ""。
//
// 保留末尾分隔符，供基于前缀的路径匹配使用。
func GetUserAutoMemPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ccode", "memory") + string(filepath.Separator)
}

// GetUserAutoMemEntrypoint 返回 ~/.ccode/memory/MEMORY.md 的路径。
func GetUserAutoMemEntrypoint() string {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsUserAutoMemPath 检查绝对路径是否位于用户级记忆目录中，用于区分用户作用域和项目作用域；
// 沙箱已经接受两者，此函数只负责路由。
func IsUserAutoMemPath(absolutePath string) bool {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return false
	}
	abs := filepath.Clean(absolutePath)
	return strings.HasPrefix(abs+string(filepath.Separator), dir)
}
