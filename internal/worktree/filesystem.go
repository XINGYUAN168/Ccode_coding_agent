// Package worktree 提供文件系统辅助能力，无需启动 Git 子进程即可读取 Git 状态。
//
// 覆盖范围：解析 .git 目录（包括工作树和子模块）、解析 HEAD，以及通过松散文件和
// packed-refs 解析引用。
//
// 正确性说明（已对照 Git 源码验证）：
// HEAD：`ref: refs/heads/<branch>\n` 或原始 SHA（refs/files-backend.c）
// Packed-refs：`<sha> <refname>\n`，跳过以 `#` 和 `^` 开头的行（packed-backend.c）
// Git 文件（工作树）：`gitdir: <path>\n`，路径可以是相对路径（setup.c）
package worktree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safeRefName 允许 ASCII 字母数字以及 '/'、'.'、'_'、'+'、'-'、'@'。它用于校验从
// .git/ 读取的引用或分支名称，防止被篡改的 HEAD 或引用文件注入路径遍历、参数前缀
// 或 shell 元字符。
var safeRefName = regexp.MustCompile(`^[a-zA-Z0-9/._+@-]+$`)

// IsSafeRefName 校验引用或分支名称能否安全用于路径拼接、Git 位置参数和 shell 命令插值。
func IsSafeRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	// 拒绝单点和空路径段。
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == "" {
			return false
		}
	}
	return safeRefName.MatchString(name)
}

var sha1Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsValidGitSha 报告 s 是否为完整长度的 SHA-1（40 位十六进制）或 SHA-256（64 位
// 十六进制）Git 对象标识。Git 从不把缩写 SHA 写入 HEAD 或引用文件。
func IsValidGitSha(s string) bool {
	return sha1Pattern.MatchString(s) || sha256Pattern.MatchString(s)
}

// ResolveGitDir 解析以 root 为根目录的仓库所使用的实际 .git 目录。它支持 .git 为
// 包含 `gitdir: <path>` 文件的工作树和子模块。root 中没有 .git 条目（不是仓库）时
// 返回 ("", nil)，调用方把空字符串视为“不是 Git 仓库”。错误只用于调用方需要处理
// 的 IO 故障（此处仅指 ENOENT 以外的文件系统错误）。
//
// 此处不做记忆化缓存（Go 调用方会在更高层缓存）。
func ResolveGitDir(root string) (string, error) {
	gitPath := filepath.Join(root, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if !st.IsDir() {
		// 工作树或子模块的 .git 是包含 `gitdir: <path>` 的文件。Git 通过 strbuf_rtrim
		//（setup.c 的 read_gitfile_gently）去掉末尾空白；strings.TrimSpace 与之等效。
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return "", err
		}
		content := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(content, "gitdir:") {
			return "", nil
		}
		rel := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
		// 相对于 .git 指针文件所在的根目录解析。
		if filepath.IsAbs(rel) {
			return rel, nil
		}
		return filepath.Clean(filepath.Join(root, rel)), nil
	}
	return gitPath, nil
}

// GetCommonDir 读取工作树 gitDir 中的 `commondir` 文件以查找共享 Git 目录。在工作树
// 中，它指向主仓库的 .git 目录。commondir 文件不存在（普通仓库）时返回 ("", nil)。
func GetCommonDir(gitDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if filepath.IsAbs(content) {
		return content, nil
	}
	return filepath.Clean(filepath.Join(gitDir, content)), nil
}

// gitHead 是 <gitDir>/HEAD 的解析结果。
type gitHead struct {
	// HEAD 位于分支上时 branch 非空。
	branch string
	// HEAD 游离（原始 SHA）或解析了非常规符号引用时，sha 非空。
	sha string
}

// readGitHead 解析 <gitDir>/HEAD 以确定当前分支或游离 SHA。HEAD 不存在或格式错误时
// 返回 (nil, nil)，调用方将其视为“不是工作树”或“不是仓库”。ENOENT 以外的 IO
// 错误会向上传递。
//
// HEAD 格式（依据 refs/files-backend.c）：
// `ref: refs/heads/<branch>\n`——位于分支上
// `ref: <other-ref>\n`——非常规符号引用（例如进行 bisect 时）
// `<hex-sha>\n`——游离 HEAD（例如进行 rebase 时）
func readGitHead(gitDir string) (*gitHead, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	content := strings.TrimSpace(string(raw))
	if strings.HasPrefix(content, "ref:") {
		ref := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
		if strings.HasPrefix(ref, "refs/heads/") {
			name := strings.TrimPrefix(ref, "refs/heads/")
			if !IsSafeRefName(name) {
				return nil, nil
			}
			return &gitHead{branch: name}, nil
		}
		// 非常规符号引用（不是本地分支）——解析为 SHA。
		if !IsSafeRefName(ref) {
			return nil, nil
		}
		sha, err := ResolveRef(gitDir, ref)
		if err != nil {
			return nil, err
		}
		return &gitHead{sha: sha}, nil
	}
	// 原始 SHA（游离 HEAD）。进行校验，防止被篡改的 HEAD 把 shell 元字符带入下游上下文。
	if !IsValidGitSha(content) {
		return nil, nil
	}
	return &gitHead{sha: content}, nil
}

// ResolveRef 把 Git 引用（例如 `refs/heads/main`）解析为提交 SHA。先检查松散引用文件，
// 再回退到 packed-refs；同时会跟随符号引用（例如 `ref: refs/remotes/origin/main`）。
//
// 对于工作树，引用位于 `commondir` 文件指向的公共 Git 目录，而不是工作树专用的
// Git 目录。这里先检查工作树 Git 目录，再回退到公共目录。
func ResolveRef(gitDir, ref string) (string, error) {
	sha, err := resolveRefInDir(gitDir, ref)
	if err != nil {
		return "", err
	}
	if sha != "" {
		return sha, nil
	}
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "", err
	}
	if commonDir != "" && commonDir != gitDir {
		return resolveRefInDir(commonDir, ref)
	}
	return "", nil
}

// resolveRefInDir 在单个 Git 目录内解析引用，不回退到 commonDir。
func resolveRefInDir(dir, ref string) (string, error) {
	// 先尝试松散引用文件。
	raw, err := os.ReadFile(filepath.Join(dir, ref))
	if err == nil {
		content := strings.TrimSpace(string(raw))
		if strings.HasPrefix(content, "ref:") {
			target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
			if !IsSafeRefName(target) {
				return "", nil
			}
			// 递归跟随符号引用链。传入 `dir`（而非 gitDir），使 resolveRef 的 commonDir
			// 回退从相同起点开始。
			return ResolveRef(dir, target)
		}
		if !IsValidGitSha(content) {
			return "", nil
		}
		return content, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	// 回退到 packed-refs。
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		spaceIdx := strings.IndexByte(line, ' ')
		if spaceIdx == -1 {
			continue
		}
		if line[spaceIdx+1:] == ref {
			sha := line[:spaceIdx]
			if !IsValidGitSha(sha) {
				return "", nil
			}
			return sha, nil
		}
	}
	return "", nil
}

// ReadRawSymref 读取原始符号引用文件，并提取已知前缀之后的分支名称。引用不存在、
// 不是符号引用或不匹配前缀时返回 ("", nil)。它只检查松散文件——packed-refs 不保存
// 符号引用。
func ReadRawSymref(gitDir, refPath, branchPrefix string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, refPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(content, "ref:") {
		return "", nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
	if !strings.HasPrefix(target, branchPrefix) {
		return "", nil
	}
	name := strings.TrimPrefix(target, branchPrefix)
	if !IsSafeRefName(name) {
		return "", nil
	}
	return name, nil
}

// GetDefaultBranch 通过读取公共 Git 目录中的符号引用 refs/remotes/origin/HEAD 来确定
// 仓库默认分支；失败时依次尝试 `main` 和 `master`，最后返回 "main"。
//
// 此过程只读取文件系统——不启动 Git 子进程，也不访问网络。
func GetDefaultBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return "main", err
	}
	if gitDir == "" {
		return "main", nil
	}
	// refs/remotes/ 位于 commonDir，而不是各工作树的 gitDir。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "main", err
	}
	if commonDir == "" {
		commonDir = gitDir
	}
	branch, err := ReadRawSymref(commonDir, "refs/remotes/origin/HEAD", "refs/remotes/origin/")
	if err != nil {
		return "main", err
	}
	if branch != "" {
		return branch, nil
	}
	for _, candidate := range []string{"main", "master"} {
		sha, err := ResolveRef(commonDir, "refs/remotes/origin/"+candidate)
		if err != nil {
			return "main", err
		}
		if sha != "" {
			return candidate, nil
		}
	}
	return "main", nil
}

// GetCurrentBranch 读取 <repoRoot>/.git/HEAD 并返回当前分支名称；HEAD 游离时返回 ""。
// 它只读取文件系统，并使用空字符串而非哨兵值 "HEAD" 来表示游离 HEAD。
func GetCurrentBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil || gitDir == "" {
		return "", err
	}
	head, err := readGitHead(gitDir)
	if err != nil || head == nil {
		return "", err
	}
	return head.branch, nil
}

// ReadWorktreeHeadSha 读取 Git 工作树目录（不是主仓库）的 HEAD SHA。与串联调用
// ResolveGitDir+readGitHead 不同，它直接把 `<worktreePath>/.git` 作为 `gitdir:` 指针
// 文件读取，不向上遍历。工作树不存在（`.git` 指针返回 ENOENT）或格式错误时返回
// ("", nil)；调用方把空字符串视为“不是有效工作树”。
//
// 性能目标：不超过 10 毫秒（只读文件系统，不启动子进程）。在包含 1600 万个对象的
// 仓库中，`git rev-parse HEAD` 仅启动开销就约为 15 毫秒。
func ReadWorktreeHeadSha(worktreePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	ptr := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(ptr, "gitdir:") {
		return "", nil
	}
	rel := strings.TrimSpace(strings.TrimPrefix(ptr, "gitdir:"))
	var gitDir string
	if filepath.IsAbs(rel) {
		gitDir = rel
	} else {
		gitDir = filepath.Clean(filepath.Join(worktreePath, rel))
	}
	head, err := readGitHead(gitDir)
	if err != nil {
		return "", err
	}
	if head == nil {
		return "", nil
	}
	if head.branch != "" {
		return ResolveRef(gitDir, "refs/heads/"+head.branch)
	}
	return head.sha, nil
}
