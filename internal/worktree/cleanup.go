package worktree

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"
)

// ephemeralWorktreePatterns 标识可以自动清理的一次性工作树。用户命名的工作树
// （例如 "my-feature"）不会匹配。
var ephemeralWorktreePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^agent-a[0-9a-f]{7}$`),
	regexp.MustCompile(`^wf_[0-9a-f]{8}-[0-9a-f]{3}-\d+$`),
	regexp.MustCompile(`^wf-\d+$`),
	regexp.MustCompile(`^bridge-[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*$`),
	regexp.MustCompile(`^job-[a-zA-Z0-9._-]{1,55}-[0-9a-f]{8}$`),
}

func isEphemeralSlug(slug string) bool {
	for _, p := range ephemeralWorktreePatterns {
		if p.MatchString(slug) {
			return true
		}
	}
	return false
}

// CleanupStaleAgentWorktrees 移除早于 cutoffDate 的陈旧 Agent 或工作流工作树。它使用
// 三层安全过滤：
//
// 1. 名称模式：仅匹配一次性 slug。2. 时间与会话：跳过当前会话和最近修改的工作树。
// 3. 变更检查：存在已跟踪变更或未推送提交时跳过。
func CleanupStaleAgentWorktrees(ctx context.Context, cutoffDate time.Time) int {
	cwd, _ := os.Getwd()
	gitRoot := FindCanonicalGitRoot(cwd)
	if gitRoot == "" {
		return 0
	}

	dir := WorktreesDir(gitRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	currentPath := ""
	if s := GetCurrentWorktreeSession(); s != nil {
		currentPath = s.WorktreePath
	}

	removed := 0
	for _, entry := range entries {
		slug := entry.Name()

		// 第一层：只处理一次性名称模式。
		if !isEphemeralSlug(slug) {
			continue
		}

		worktreePath := WorktreePathFor(gitRoot, slug)
		if currentPath == worktreePath {
			continue
		}

		// 第二层：检查时间。
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoffDate) {
			continue
		}

		// 第三层：采用保守策略检查变更。陈旧且已崩溃的 Agent 工作树中的未跟踪文件属于
		// 构建产物；使用 -uno 跳过未跟踪文件扫描，在大型仓库中可快 5–10 倍。
		statusOut, _, statusCode := runGit(ctx, worktreePath,
			"--no-optional-locks", "status", "--porcelain", "-uno")
		if statusCode != 0 || strings.TrimSpace(statusOut) != "" {
			continue
		}

		unpushedOut, _, unpushedCode := runGit(ctx, worktreePath,
			"rev-list", "--max-count=1", "HEAD", "--not", "--remotes")
		if unpushedCode != 0 || strings.TrimSpace(unpushedOut) != "" {
			continue
		}

		if RemoveAgentWorktree(ctx, worktreePath, WorktreeBranchName(slug), gitRoot) {
			removed++
		}
	}

	if removed > 0 {
		runGit(ctx, gitRoot, "worktree", "prune")
	}
	return removed
}

// StartCleanupLoop 在后台 goroutine 中定期清理陈旧工作树。此函数立即返回；ctx 被取消
// 时 goroutine 退出。
func StartCleanupLoop(ctx context.Context) {
	interval := GetStaleCleanupInterval()
	if interval <= 0 {
		return
	}
	cutoffHours := GetStaleCutoffHours()

	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-time.Duration(cutoffHours) * time.Hour)
				CleanupStaleAgentWorktrees(ctx, cutoff)
			}
		}
	}()
}
