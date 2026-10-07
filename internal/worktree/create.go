package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WorktreesDir 返回所有由 Ccode 管理的工作树所在目录，即仓库根目录内一个已加入
// .gitignore 的目录。
func WorktreesDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".ccode", "worktrees")
}

// WorktreePathFor 返回 slug 对应的工作树目录路径，并先展平嵌套 slug
// （例如把 `team/alice` 转为 `team+alice`）。
func WorktreePathFor(repoRoot, slug string) string {
	return filepath.Join(WorktreesDir(repoRoot), FlattenSlug(slug))
}

// CreateResult 是 getOrCreateWorktree 的结果。Existed=true 表示快速恢复了已有工作树
// （跳过 `git worktree add` 和 performPostCreationSetup）；Existed=false 表示刚创建
// 工作树，调用方仍需执行创建后设置。
type CreateResult struct {
	WorktreePath   string
	WorktreeBranch string
	HeadCommit     string
	BaseBranch     string // Existed=true 时为空（恢复路径不计算 baseBranch）
	Existed        bool
}

// getOrCreateWorktree 在 <repoRoot>/.ccode/worktrees/ 下为给定 slug 创建新的 Git 工作树，
// 如果工作树已经存在则恢复它。
//
// 快速恢复路径：ReadWorktreeHeadSha 直接读取 .git 指针文件（不启动子进程，也不向上
// 遍历）。对于包含 1600 万个对象的仓库，这可以省去每次恢复时 `git fetch` 扫描
// 提交图所需的约 6–8 秒。
//
// 创建路径：只读取文件系统来解析默认分支（本地已知 origin/<default> 时不执行
// `git fetch`），然后运行 `git worktree add -B worktree-<flat> <path> <baseBranch>`。
//
// `-B`（大写而非 `-b`）会重置已删除工作树目录遗留的孤立分支，省去每次创建时
// 运行 `git branch -D` 子进程。
func getOrCreateWorktree(ctx context.Context, repoRoot, slug string) (*CreateResult, error) {
	worktreePath := WorktreePathFor(repoRoot, slug)
	worktreeBranch := WorktreeBranchName(slug)

	// 快速恢复路径：工作树已存在 → 跳过 fetch 和 add。
	existingHead, err := ReadWorktreeHeadSha(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("read worktree HEAD: %w", err)
	}
	if existingHead != "" {
		return &CreateResult{
			WorktreePath:   worktreePath,
			WorktreeBranch: worktreeBranch,
			HeadCommit:     existingHead,
			Existed:        true,
		}, nil
	}

	if err := os.MkdirAll(WorktreesDir(repoRoot), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees dir: %w", err)
	}

	// 解析 baseBranch 和 baseSha。如果本地已有 origin/<default>，则跳过 fetch——大型仓库
	// 在访问网络前，仅扫描本地提交图就会耗费约 6–8 秒。基线稍旧也可以接受；用户需要
	// 最新版本时可在工作树中自行拉取。
	defaultBranch, err := GetDefaultBranch(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("get default branch: %w", err)
	}
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w", err)
	}
	var baseBranch, baseSha string
	if gitDir != "" {
		baseSha, _ = ResolveRef(gitDir, "refs/remotes/origin/"+defaultBranch)
	}
	if baseSha != "" {
		baseBranch = "origin/" + defaultBranch
	} else {
		// 本地没有 origin/<default>——尝试 `git fetch origin <default>`。如果失败（离线或
		// 没有远程仓库），则回退到 HEAD，至少可以从当前工作树提交创建分支，而不是报错。
		_, _, fetchCode := runGit(ctx, repoRoot, "fetch", "origin", defaultBranch)
		if fetchCode == 0 {
			baseBranch = "origin/" + defaultBranch
		} else {
			baseBranch = "HEAD"
		}
		// 通过子进程解析所选 baseBranch 的 SHA——若不增加额外处理，resolveRef 无法在
		// 工作树共享的 commonDir 中找到 FETCH_HEAD 或 HEAD。
		stdout, _, shaCode := runGit(ctx, repoRoot, "rev-parse", baseBranch)
		if shaCode != 0 {
			return nil, fmt.Errorf(`failed to resolve base branch %q: git rev-parse failed`, baseBranch)
		}
		baseSha = trimNewline(stdout)
	}

	// `-B`（大写）会重置已删除工作树目录遗留的孤立分支；`-b` 则会报错。
	_, stderr, code := runGit(ctx, repoRoot, "worktree", "add", "-B", worktreeBranch, worktreePath, baseBranch)
	if code != 0 {
		return nil, fmt.Errorf("failed to create worktree: %s", stderr)
	}

	return &CreateResult{
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		HeadCommit:     baseSha,
		BaseBranch:     baseBranch,
		Existed:        false,
	}, nil
}

// trimNewline 去掉末尾的 CR/LF；对于单行命令输出，它等同于 .trim。保留为本地实现，
// 避免只为此功能导入 strings。
func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
