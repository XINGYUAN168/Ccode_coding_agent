package worktree

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxWorktreeSlugLength 把工作树名称限制为最多 64 个字符。
const MaxWorktreeSlugLength = 64

// validWorktreeSlugSegment 是用 '/' 拆分 slug 后逐段应用的允许列表。
var validWorktreeSlugSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidateWorktreeSlug 校验工作树 slug，以防止路径遍历和目录逃逸。slug 通过
// filepath.Join 拼入 `.ccode/worktrees/<slug>`，该函数会规范化 `.` 路径段，因此
// `./././target` 会逃出 worktrees 目录。类似地，以 `/` 或 `C:\` 开头的绝对路径会
// 完全丢弃前缀。
//
// 允许使用正斜杠嵌套（例如 `asm/feature-foo`）；每个路径段都单独按允许列表校验，
// 因此 `.`、`..` 路径段和盘符字符仍会被拒绝。
//
// 此函数同步返回——调用方依赖它在任何副作用（Git 命令、钩子执行、chdir）之前完成。
func ValidateWorktreeSlug(slug string) error {
	if len(slug) > MaxWorktreeSlugLength {
		return fmt.Errorf(
			"Invalid worktree name: must be %d characters or fewer (got %d)",
			MaxWorktreeSlugLength, len(slug),
		)
	}
	// 前导或末尾的 `/` 会使 filepath.Join 生成绝对路径或悬空路径段。拆分并校验每个
	// 路径段可拒绝这两种情况（空路径段无法通过正则），同时允许 `user/feature`。
	for _, segment := range strings.Split(slug, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf(
				`Invalid worktree name %q: must not contain "." or ".." path segments`,
				slug,
			)
		}
		if !validWorktreeSlugSegment.MatchString(segment) {
			return fmt.Errorf(
				`Invalid worktree name %q: each "/"-separated segment must be non-empty and contain only letters, digits, dots, underscores, and dashes`,
				slug,
			)
		}
	}
	return nil
}

// FlattenSlug 把嵌套 slug（`user/feature` → `user+feature`）展平后同时用于分支名和
// 目录路径。在任一位置保留嵌套都不安全：
// Git 引用：`worktree-user`（文件）与 `worktree-user/feature`（需要目录）会形成
// Git 拒绝的目录/文件冲突。
// 目录：`.ccode/worktrees/user/feature/` 位于 `user` 工作树内；对父目录执行
// `git worktree remove` 会删除含有未提交工作的子目录。
//
// `+` 在 Git 分支名和文件系统路径中有效，但不在 slug 路径段允许列表
// （[a-zA-Z0-9._-]）中，因此该映射是单射。
func FlattenSlug(slug string) string {
	return strings.ReplaceAll(slug, "/", "+")
}

// WorktreeBranchName 返回与 slug 关联的工作树 Git 分支名，格式为
// "worktree-<flattenedSlug>"。
func WorktreeBranchName(slug string) string {
	return "worktree-" + FlattenSlug(slug)
}
