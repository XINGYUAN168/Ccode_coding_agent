package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIsEphemeralSlug(t *testing.T) {
	tests := []struct {
		slug     string
		expected bool
	}{
		{"agent-a1234567", true},
		{"agent-aabcdef0", true},
		{"wf_12345678-abc-1", true},
		{"wf_12345678-abc-42", true},
		{"wf-1", true},
		{"wf-99", true},
		{"bridge-abc", true},
		{"bridge-abc_def-ghi", true},
		{"job-mytemplate-12345678", true},
		// 以下内容不应匹配。
		{"my-feature", false},
		{"agent-too-long", false},
		{"agent-a123", false},     // 长度过短。
		{"agent-aGGGGGGG", false}, // 不是十六进制。
		{"wf_short", false},
	}
	for _, tt := range tests {
		got := isEphemeralSlug(tt.slug)
		if got != tt.expected {
			t.Errorf("isEphemeralSlug(%q) = %v, want %v", tt.slug, got, tt.expected)
		}
	}
}

func TestCleanupStaleAgentWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// 添加虚拟远程仓库，使 --not --remotes 生效（工作树 HEAD 可从远程仓库到达，因此
	// rev-list 返回空）。显式推送 HEAD，使测试不依赖用户的默认分支。
	bare := t.TempDir()
	exec.Command("git", "init", "--bare", bare).Run()
	exec.Command("git", "-C", repo, "remote", "add", "origin", bare).Run()
	if out, err := exec.Command("git", "-C", repo, "push", "origin", "HEAD:refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("push test remote: %v\n%s", err, out)
	}

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 创建一次性工作树。
	result, err := CreateAgentWorktree(context.Background(), "agent-aaaaaaaa")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 把修改时间设为 25 小时前。
	past := time.Now().Add(-25 * time.Hour)
	os.Chtimes(result.WorktreePath, past, past)

	// 使用 24 小时前的截止时间清理时，应将其移除。
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	// 目录应已移除。
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("stale worktree should be removed")
	}
}

func TestCleanupStaleAgentWorktrees_SkipsUserNamed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 创建用户命名的工作树（非一次性）。
	_, err := getOrCreateWorktree(context.Background(), repo, "my-feature")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	wtPath := WorktreePathFor(repo, "my-feature")

	// 把修改时间设为过去。
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(wtPath, past, past)

	// 清理不应移除用户命名的工作树。
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 0 {
		t.Fatal("user-named worktree should not be cleaned up")
	}

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatal("user-named worktree should still exist")
	}
}

func TestCleanupStaleAgentWorktrees_SkipsDirtyWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-abbbbbbb")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 制造未提交变更。
	os.WriteFile(filepath.Join(result.WorktreePath, "dirty.txt"), []byte("dirty"), 0o644)
	exec.Command("git", "-C", result.WorktreePath, "add", ".").Run()

	// 把修改时间设为过去。
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(result.WorktreePath, past, past)

	// 清理应跳过有变更的工作树。
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 0 {
		t.Fatal("dirty worktree should not be cleaned up")
	}
}
