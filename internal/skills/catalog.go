package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Catalog 是所有已加载 Skill 的内存注册表。第一阶段的条目只包含 frontmatter
// （PromptBody 为空、BodyLoaded 为 false）；每次调用 GetFull 都会触发第二阶段的
// 正文读取（热重载）。
type Catalog struct {
	skills      map[string]*Skill
	sources     map[string]string // Skill 名称 → "builtin" | "user" | "project" | 绝对路径
	workDir     string            // 记住此目录，使 Reload 可以重新扫描相同的三层来源
	hasReload   bool
	dirModTimes map[string]time.Time // Skill 目录路径 → 最近一次记录的修改时间
}

func NewCatalog() *Catalog {
	return &Catalog{
		skills:      make(map[string]*Skill),
		sources:     make(map[string]string),
		dirModTimes: make(map[string]time.Time),
	}
}

// Register 向目录中添加（或覆盖）一个 Skill。来源标签会由 /skills 展示。
func (c *Catalog) Register(s *Skill, source string) {
	c.skills[s.Meta.Name] = s
	c.sources[s.Meta.Name] = source
}

// Get 返回第一阶段的 Skill（仅包含 frontmatter）。如果目录以第一阶段模式加载，
// PromptBody 可能为空。
func (c *Catalog) Get(name string) *Skill {
	return c.skills[name]
}

// GetFull 返回已加载正文的 Skill。对于磁盘上的 Skill，每次调用都会重新读取正文
// （热重载）；对于内嵌 Skill，正文已经在内存中，因此会直接命中缓存。读取失败时，
// 保留之前缓存的正文并返回错误。
func (c *Catalog) GetFull(name string) (*Skill, error) {
	skill, ok := c.skills[name]
	if !ok {
		return nil, fmt.Errorf("unknown skill: %s", name)
	}
	if skill.SourceDir == "" {
		// 内嵌 Skill 的正文已在启动时加载，无需刷新。
		return skill, nil
	}
	if err := loadSkillBody(skill); err != nil {
		// 如果已有缓存正文就保留，由调用方决定是显示错误还是继续回退处理。
		if skill.PromptBody == "" {
			return nil, err
		}
		return skill, err
	}
	return skill, nil
}

// List 返回所有已加载 Skill 的元数据。结果采用 map 迭代顺序（未排序），需要稳定
// 顺序的调用方应按 Name 排序。
func (c *Catalog) List() []SkillMeta {
	result := make([]SkillMeta, 0, len(c.skills))
	for _, s := range c.skills {
		result = append(result, s.Meta)
	}
	return result
}

// Source 返回 Skill 的来源标签（"builtin"、"user"、"project" 或路径）。
// Skill 尚未加载时返回 ""。
func (c *Catalog) Source(name string) string {
	return c.sources[name]
}

// Reload 重新扫描三层来源（builtin + user + project），并原地重建目录。
// `/skills reload` 和测试会使用此方法。
func (c *Catalog) Reload(workDir string) {
	fresh := LoadCatalog(workDir)
	c.skills = fresh.skills
	c.sources = fresh.sources
	c.workDir = fresh.workDir
	c.dirModTimes = fresh.dirModTimes
}

// NeedsReload 检查自上次加载目录以来，Skill 目录的修改时间是否发生变化。修改时间
// 改变表示有 Skill 被添加或移除（已有 Skill 内的文件改动由 GetFull 每次调用时重新
// 读取来处理）。
func (c *Catalog) NeedsReload() bool {
	for dir, recorded := range c.dirModTimes {
		info, err := os.Stat(dir)
		if err != nil {
			if recorded.IsZero() {
				continue
			}
			return true // 目录已消失
		}
		if !info.ModTime().Equal(recorded) {
			return true
		}
	}
	// 检查上次加载后是否新建了目录。
	dirs := skillDirPaths(c.workDir)
	for _, dir := range dirs {
		if _, tracked := c.dirModTimes[dir]; !tracked {
			if info, err := os.Stat(dir); err == nil && !info.ModTime().IsZero() {
				return true
			}
		}
	}
	return false
}

// snapshotDirModTimes 记录所有 Skill 目录当前的修改时间。
func (c *Catalog) snapshotDirModTimes() {
	c.dirModTimes = make(map[string]time.Time)
	for _, dir := range skillDirPaths(c.workDir) {
		info, err := os.Stat(dir)
		if err != nil {
			c.dirModTimes[dir] = time.Time{}
			continue
		}
		c.dirModTimes[dir] = info.ModTime()
	}
}

// skillDirPaths 返回用户全局和项目级 Skill 目录路径。
func skillDirPaths(workDir string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".ccode", "skills"))
	}
	if workDir != "" {
		dirs = append(dirs, filepath.Join(workDir, ".ccode", "skills"))
	}
	return dirs
}

// LoadCatalog 合并三层来源来构建第一阶段目录，后加载的来源会按名称覆盖先加载的来源
// （project 优先于 user，user 优先于 builtin）：
//  1. internal/skills/builtins/*（通过 go:embed 内嵌，优先级最低）
//  2. ~/.ccode/skills/         （用户全局）
//  3. $workDir/.ccode/skills/  （项目级，优先级最高）
//
// 此阶段只读取 frontmatter；调用 GetFull 前 PromptBody 保持为空。单个 Skill 解析
// 失败时会被静默跳过——一个错误文件不能导致整个目录不可用。
func LoadCatalog(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir

	// 第一层：内嵌 Skill。
	for _, s := range LoadBuiltins() {
		c.Register(s, "builtin")
	}

	// 第二层：用户全局 Skill。
	if home, err := os.UserHomeDir(); err == nil {
		loadTierInto(c, filepath.Join(home, ".ccode", "skills"), "user")
	}

	// 第三层：项目级 Skill。
	loadTierInto(c, filepath.Join(workDir, ".ccode", "skills"), "project")

	c.snapshotDirModTimes()
	return c
}

// LoadFromDirectory 把 dir 的每个子目录作为一个 Skill 加载。它用于测试，以及只想
// 加载单层来源的一次性调用方。正文会立即读取（不分两个阶段），使现有访问
// skill.PromptBody 的测试代码可以继续工作。
func LoadFromDirectory(dir string) (*Catalog, error) {
	c := NewCatalog()
	loadTierEager(c, dir, dir)
	return c, nil
}

// LoadSkills 是为兼容仍会立即预加载正文的旧代码而保留的双层加载器。新调用方应
// 使用 LoadCatalog + GetFull。顺序：用户全局 → 项目级。
func LoadSkills(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir
	if home, err := os.UserHomeDir(); err == nil {
		loadTierEager(c, filepath.Join(home, ".ccode", "skills"), "user")
	}
	loadTierEager(c, filepath.Join(workDir, ".ccode", "skills"), "project")
	return c
}

// loadTierInto 遍历单层来源，把每个子目录注册为第一阶段 Skill。单个条目的错误会被忽略。
func loadTierInto(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, err := parseFrontmatterOnly(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		c.Register(skill, source)
	}
}

// loadTierEager 与 loadTierInto 类似，但还会读取正文。旧版 LoadSkills 和
// LoadFromDirectory 使用它来保留原有行为。
func loadTierEager(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		skill, err := parseFrontmatterOnly(path)
		if err != nil {
			continue
		}
		_ = loadSkillBody(skill)
		c.Register(skill, source)
	}
}
