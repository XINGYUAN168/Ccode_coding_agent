package worktree

import (
	"context"
	"strconv"
	"strings"
)

// ChangeSummary 保存 countWorktreeChanges 的结果。
type ChangeSummary struct {
	ChangedFiles int
	Commits      int
}

// HasWorktreeChanges 在工作树存在未提交变更或 headCommit 之后的新提交时返回 true。
// Git 操作失败时按保守策略返回 true。
func HasWorktreeChanges(ctx context.Context, worktreePath, headCommit string) bool {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return true // 失败时采用保守策略。
	}
	if strings.TrimSpace(stdout) != "" {
		return true
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", headCommit+"..HEAD")
	if code != 0 {
		return true // 失败时采用保守策略。
	}
	n, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return true // 失败时采用保守策略。
	}
	return n > 0
}

// CountWorktreeChanges 返回详细的变更摘要；无法可靠确定状态时返回 nil。把它用作安全
// 门禁的调用方必须将 nil 视为“状态未知，假定不安全”（失败时采用保守策略）。
func CountWorktreeChanges(ctx context.Context, worktreePath, originalHeadCommit string) *ChangeSummary {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return nil // 失败时采用保守策略。
	}
	changedFiles := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			changedFiles++
		}
	}

	if originalHeadCommit == "" {
		// 没有基线提交就无法统计提交数，失败时采用保守策略。
		return nil
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", originalHeadCommit+"..HEAD")
	if code != 0 {
		return nil // 失败时采用保守策略。
	}
	commits, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return nil
	}

	return &ChangeSummary{
		ChangedFiles: changedFiles,
		Commits:      commits,
	}
}
