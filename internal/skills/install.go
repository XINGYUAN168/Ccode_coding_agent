package skills

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SkillSource 描述从何处拉取 Skill。它最终总会规范化为 GitHub Contents API 路径，
// 因为 skills.sh 只是一个指向 GitHub 目录树的注册中心。
type SkillSource struct {
	Owner    string
	Repo     string
	Ref      string // 分支或标签；未指定时为 "main"
	Subpath  string // 仓库内指向 Skill 目录的路径（末尾不带 /）
	Name     string // Skill 名称（即 Subpath 的最后一段）
	Original string // 用户提供的 URL，用于错误消息
}

// ParseSkillURL 接受三种 URL 形式：
//
//  1. 形式一：https://www.skills.sh/<owner>/<repo>/<skill-name>
//     ——假定 Skill 位于仓库内的 "skills/<skill-name>"（anthropics/skills 约定）
//  2. 形式二：https://github.com/<owner>/<repo>/tree/<ref>/<subpath>
//     ——直接指向子目录树的 URL；最后一段是 Skill 名称
//  3. 形式三：https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<subpath>/SKILL.md
//     ——原始文件 URL；把父目录视为 Skill 子路径
//
// 返回完全解析的 SkillSource；如果 URL 不符合上述任一形式，则返回错误。
func ParseSkillURL(raw string) (*SkillSource, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http(s) URLs are supported")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch u.Host {
	case "www.skills.sh", "skills.sh":
		if len(parts) < 3 {
			return nil, fmt.Errorf("skills.sh URL must be /<owner>/<repo>/<skill-name>")
		}
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      "main",
			Subpath:  "skills/" + strings.Join(parts[2:], "/"),
			Name:     parts[len(parts)-1],
			Original: raw,
		}, nil

	case "github.com":
		// 预期格式：/<owner>/<repo>/tree/<ref>/<...subpath>
		if len(parts) < 5 || parts[2] != "tree" {
			return nil, fmt.Errorf("github.com URL must be /<owner>/<repo>/tree/<ref>/<subpath>")
		}
		sub := strings.Join(parts[4:], "/")
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      parts[3],
			Subpath:  sub,
			Name:     parts[len(parts)-1],
			Original: raw,
		}, nil

	case "raw.githubusercontent.com":
		// 预期格式：/<owner>/<repo>/<ref>/<...subpath>/SKILL.md
		if len(parts) < 4 {
			return nil, fmt.Errorf("raw.githubusercontent.com URL too short")
		}
		// 去掉末尾文件名，使 Subpath 结束于 Skill 目录。
		subParts := parts[3:]
		if last := subParts[len(subParts)-1]; strings.Contains(last, ".") {
			subParts = subParts[:len(subParts)-1]
		}
		if len(subParts) == 0 {
			return nil, fmt.Errorf("raw URL missing skill subpath")
		}
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      parts[2],
			Subpath:  strings.Join(subParts, "/"),
			Name:     subParts[len(subParts)-1],
			Original: raw,
		}, nil
	}
	return nil, fmt.Errorf("unsupported host %q (try skills.sh or github.com)", u.Host)
}

// contentEntry 保存所需的 GitHub Contents API 响应字段。Type 可为 "file"、
// "dir"、"symlink" 或 "submodule"；这里只跟随文件和目录。
type contentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Size        int    `json:"size"`
}

// installLimits 限制从远程来源拉取的数据量。单个 Skill 安装包通常很小
// （SKILL.md 加上少量参考文件）；超出限制通常意味着 URL 错误或来源不可信。
const (
	maxFileSize       = 1 << 20 // 每个文件 1 MiB
	maxTotalSize      = 8 << 20 // 每个 Skill 8 MiB
	maxFileCount      = 64
	maxRecursionDepth = 4
	httpTimeout       = 30 * time.Second
)

// fetcher 集中处理 HTTP 调用，使测试可以替换底层客户端，并统一应用请求头和超时。
type fetcher struct {
	client  *http.Client
	apiBase string // "https://api.github.com"——测试中可覆盖
}

func newFetcher() *fetcher {
	return &fetcher{
		client:  &http.Client{Timeout: httpTimeout},
		apiBase: "https://api.github.com",
	}
}

func (f *fetcher) listContents(src *SkillSource, subpath string) ([]contentEntry, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		f.apiBase, src.Owner, src.Repo, subpath, url.QueryEscape(src.Ref))
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccode-install-skill")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contents API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		// 受到速率限制时显示响应正文，让用户看到 GitHub 错误消息。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github API forbidden (rate-limited?): %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned %d for %s", resp.StatusCode, endpoint)
	}

	// 该端点对目录返回数组，对文件返回单个对象；先解码成通用值，再分别处理。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
	if err != nil {
		return nil, fmt.Errorf("read contents response: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("github returned empty body")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var entries []contentEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse dir listing: %w", err)
		}
		return entries, nil
	}
	var single contentEntry
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("parse file metadata: %w", err)
	}
	return []contentEntry{single}, nil
}

// fetchBlob 下载单个文件的字节。存在内联的 base64 `content` 字段时优先使用它
// （比发起第二次请求开销更低）；对于二进制文件或大于 1 MB 的文件则回退到
// download_url 字段。
func (f *fetcher) fetchBlob(e contentEntry) ([]byte, error) {
	if e.Size > maxFileSize {
		return nil, fmt.Errorf("file %s too large: %d bytes (max %d)", e.Path, e.Size, maxFileSize)
	}
	if e.Encoding == "base64" && e.Content != "" {
		clean := strings.ReplaceAll(e.Content, "\n", "")
		out, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("decode base64 for %s: %w", e.Path, err)
		}
		return out, nil
	}
	if e.DownloadURL == "" {
		return nil, fmt.Errorf("no download_url for %s", e.Path)
	}
	req, _ := http.NewRequest("GET", e.DownloadURL, nil)
	req.Header.Set("User-Agent", "ccode-install-skill")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", e.DownloadURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
}

// InstallReport 汇总 Install 调用执行的操作，供工具返回展示并由测试使用。
type InstallReport struct {
	SkillName  string
	TargetDir  string
	FileCount  int
	TotalBytes int64
}

// Install 把 src 指向的 Skill 拉取到 installRoot/<src.Name>/。installRoot 应为
// 用户全局 Skill 层（~/.ccode/skills/），使安装结果可以跨项目复用。
//
// 写入在目录层面是原子的：先暂存到同级临时目录，再重命名到目标位置。发生部分失败
// 时，installRoot 保持不变。
func Install(src *SkillSource, installRoot string) (*InstallReport, error) {
	return installWith(newFetcher(), src, installRoot)
}

func installWith(f *fetcher, src *SkillSource, installRoot string) (*InstallReport, error) {
	if src == nil {
		return nil, fmt.Errorf("nil source")
	}
	if err := validateSkillName(src.Name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create install root: %w", err)
	}
	staging, err := os.MkdirTemp(installRoot, ".install-"+src.Name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	cleanupStaging := func() { _ = os.RemoveAll(staging) }

	report := &InstallReport{SkillName: src.Name}
	if err := walkAndDownload(f, src, src.Subpath, staging, report, 0); err != nil {
		cleanupStaging()
		return nil, err
	}
	if !hasSkillManifest(staging) {
		cleanupStaging()
		return nil, fmt.Errorf("downloaded tree missing SKILL.md or skill.yaml — not a skill?")
	}

	final := filepath.Join(installRoot, src.Name)
	if _, err := os.Stat(final); err == nil {
		// 先移除已有安装再覆盖。用户已明确要求安装，因此假定其需要最新版本。
		if err := os.RemoveAll(final); err != nil {
			cleanupStaging()
			return nil, fmt.Errorf("remove old install: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		cleanupStaging()
		return nil, fmt.Errorf("promote staging dir: %w", err)
	}
	report.TargetDir = final
	return report, nil
}

// walkAndDownload 在 localDir 下还原 GitHub 上的目录树，并按安装限制统计文件数和字节数。
func walkAndDownload(f *fetcher, src *SkillSource, subpath, localDir string, report *InstallReport, depth int) error {
	if depth > maxRecursionDepth {
		return fmt.Errorf("install tree too deep (>%d levels)", maxRecursionDepth)
	}
	entries, err := f.listContents(src, subpath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if report.FileCount >= maxFileCount {
			return fmt.Errorf("install file count limit (%d) reached", maxFileCount)
		}
		// `Name` 是叶节点名称；即使 GitHub API 本身不会返回 "../"，仍需防范路径遍历。
		if strings.Contains(e.Name, "..") || strings.ContainsAny(e.Name, "/\\") {
			return fmt.Errorf("suspicious entry name: %q", e.Name)
		}
		target := filepath.Join(localDir, e.Name)
		switch e.Type {
		case "file":
			data, err := f.fetchBlob(e)
			if err != nil {
				return err
			}
			if int64(report.TotalBytes)+int64(len(data)) > maxTotalSize {
				return fmt.Errorf("install total size limit (%d bytes) reached", maxTotalSize)
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
			report.FileCount++
			report.TotalBytes += int64(len(data))
		case "dir":
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := walkAndDownload(f, src, e.Path, target, report, depth+1); err != nil {
				return err
			}
		default:
			// 静默跳过符号链接和子模块——结构正确的 Skill 中不应出现它们。
		}
	}
	return nil
}

// hasSkillManifest 验证暂存目录树根目录包含 SKILL.md 或 skill.yaml，用于预先防止
// “URL 指向错误子目录”的情况。
func hasSkillManifest(dir string) bool {
	for _, name := range []string{"SKILL.md", "skill.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// validateSkillName 允许 kebab-case 和 snake_case；禁止路径遍历、前导点，以及任何
// 会让 shell 产生意外解释的字符。
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("empty skill name")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("skill name cannot start with '.'")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("skill name %q contains invalid char %q (use a-z 0-9 - _)", name, r)
		}
	}
	return nil
}

// UserSkillsRoot 返回 ~/.ccode/skills，并在需要时创建父目录，避免调用方重复处理。
func UserSkillsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	root := filepath.Join(home, ".ccode", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}
