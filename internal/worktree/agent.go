package worktree

import (
	"context"
	"os"
	"time"
)

// AgentWorktreeResult 保存 CreateAgentWorktree 的结果。
type AgentWorktreeResult struct {
	WorktreePath   string
	WorktreeBranch string
	HeadCommit     string
	GitRoot        string
}

// CreateAgentWorktree 为子 Agent 创建轻量工作树。与 CreateWorktreeForSession 不同，
// 它不会修改全局会话状态（currentWorktreeSession、process.chdir、项目配置）。
func CreateAgentWorktree(ctx context.Context, slug string) (*AgentWorktreeResult, error) {
	if err := ValidateWorktreeSlug(slug); err != nil {
		return nil, err
	}

	cwd, _ := os.Getwd()
	gitRoot := FindCanonicalGitRoot(cwd)
	if gitRoot == "" {
		return nil, &worktreeError{msg: "cannot create agent worktree: not in a git repository"}
	}

	result, err := getOrCreateWorktree(ctx, gitRoot, slug)
	if err != nil {
		return nil, err
	}

	if !result.Existed {
		performPostCreationSetup(ctx, gitRoot, result.WorktreePath)
	} else {
		// 推进修改时间，避免周期性过期清理把它视为陈旧工作树。
		now := time.Now()
		_ = os.Chtimes(result.WorktreePath, now, now)
	}

	return &AgentWorktreeResult{
		WorktreePath:   result.WorktreePath,
		WorktreeBranch: result.WorktreeBranch,
		HeadCommit:     result.HeadCommit,
		GitRoot:        gitRoot,
	}, nil
}

// RemoveAgentWorktree 移除由 CreateAgentWorktree 创建的工作树。
func RemoveAgentWorktree(ctx context.Context, worktreePath, worktreeBranch, gitRoot string) bool {
	if gitRoot == "" {
		return false
	}

	_, _, code := runGit(ctx, gitRoot, "worktree", "remove", "--force", worktreePath)
	if code != 0 {
		return false
	}

	if worktreeBranch != "" {
		// 等待 Git 释放锁文件。
		time.Sleep(100 * time.Millisecond)
		runGit(ctx, gitRoot, "branch", "-D", worktreeBranch)
	}
	return true
}
