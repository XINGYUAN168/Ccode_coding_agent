package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseFrontmatterOnly 执行第一阶段加载：只读取足以从 Skill 文件中提取 SkillMeta
// 的内容，并让 PromptBody 保持为空。开销足够低，可在启动时处理数百个 Skill。
//
// 支持以下两种布局：
//   - <dir>/skill.yaml（可选配 prompt.md，第一阶段忽略）
//   - 带有 `---` YAML frontmatter 的 <dir>/SKILL.md
func parseFrontmatterOnly(dir string) (*Skill, error) {
	yamlPath := filepath.Join(dir, "skill.yaml")
	if data, err := os.ReadFile(yamlPath); err == nil {
		var meta SkillMeta
		if err := yaml.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("parse skill.yaml: %w", err)
		}
		applyMetaDefaults(&meta, dir, "")
		return &Skill{
			Meta:        meta,
			SourceDir:   dir,
			IsDirectory: true,
			BodyLoaded:  false,
		}, nil
	}

	mdPath := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, fmt.Errorf("no skill.yaml or SKILL.md: %w", err)
	}
	meta, _ := splitFrontmatter(string(data))
	applyMetaDefaults(&meta, dir, string(data))
	return &Skill{
		Meta:        meta,
		SourceDir:   dir,
		IsDirectory: true,
		BodyLoaded:  false,
	}, nil
}

// loadSkillBody 读取已经解析过 frontmatter 的 Skill 正文。每次调用 Skill 时，
// Catalog.GetFull 都会调用它（热重载）。出现任何读取或解析错误时，已有的
// PromptBody 保持不变并返回错误，使调用方可以回退到缓存版本。
func loadSkillBody(skill *Skill) error {
	yamlPath := filepath.Join(skill.SourceDir, "skill.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		promptPath := filepath.Join(skill.SourceDir, "prompt.md")
		body, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("read prompt.md: %w", err)
		}
		skill.PromptBody = string(body)
		skill.BodyLoaded = true
		return nil
	}

	mdPath := filepath.Join(skill.SourceDir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	_, body := splitFrontmatter(string(data))
	skill.PromptBody = body
	skill.BodyLoaded = true
	return nil
}

// loadSkillFromBytes 从内存字节中解析 Skill（用于没有磁盘目录可供重新读取的
// go:embed 内嵌 Skill）。
func loadSkillFromBytes(name string, mdBytes []byte) (*Skill, error) {
	meta, body := splitFrontmatter(string(mdBytes))
	if meta.Name == "" {
		meta.Name = name
	}
	applyMetaDefaults(&meta, name, string(mdBytes))
	return &Skill{
		Meta:        meta,
		PromptBody:  body,
		SourceDir:   "", // 内嵌 Skill——没有源目录
		IsDirectory: false,
		BodyLoaded:  true,
	}, nil
}

// splitFrontmatter 把 YAML frontmatter 与 Markdown 正文分开。如果不存在 `---`
// frontmatter，则返回零值 meta。
func splitFrontmatter(content string) (SkillMeta, string) {
	var meta SkillMeta
	body := content

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &meta); err == nil {
				body = strings.TrimSpace(parts[2])
			}
		}
	}
	return meta, body
}

// applyMetaDefaults 沿用旧版 parseSkillMD 的方式补齐名称和描述等默认值：
//   - 缺少名称 → 从目录基本名派生（小写 + kebab 格式）
//   - 缺少描述 → 取正文中第一个非空、非标题行
//   - 缺少 Mode → "inline"
//   - 缺少 ForkContext → "none"（仅当 Mode == "fork" 时有意义）
func applyMetaDefaults(meta *SkillMeta, dirOrName, body string) {
	if meta.Name == "" {
		base := filepath.Base(dirOrName)
		meta.Name = strings.ToLower(strings.ReplaceAll(base, " ", "-"))
	}
	if meta.Description == "" && body != "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "---") {
				meta.Description = line
				break
			}
		}
	}
	if meta.Mode == "" {
		if meta.Context == "fork" {
			meta.Mode = "fork"
		} else {
			meta.Mode = "inline"
		}
	}
	if meta.IsFork() && meta.ForkContext == "" {
		meta.ForkContext = "none"
	}
}
