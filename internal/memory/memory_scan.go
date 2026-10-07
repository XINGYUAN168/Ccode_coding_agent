package memory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryHeader 表示一份已扫描记忆文件的元数据。
type MemoryHeader struct {
	Filename    string     // 相对于 memoryDir 的路径
	FilePath    string     // 绝对路径
	Scope       string     // "user" 或 "project"；调用方不关心时为空
	MtimeMs     int64      // 修改时间，即 Unix 纪元后的毫秒数
	Description string     // frontmatter 说明；缺失时为 ""
	Type        MemoryType // frontmatter 类型；无法识别时为 ""
}

// MaxMemoryFiles 限制展示给模型的记忆数量。
// FrontmatterMaxLines 限制为解析文件头而读取的每个文件行数。
const (
	MaxMemoryFiles      = 200
	FrontmatterMaxLines = 30
)

// ScanMemoryFiles 扫描记忆目录中的 .md 文件并读取其 frontmatter，返回按从新到旧排序、
// 最多 MaxMemoryFiles 项的文件头列表。findRelevantMemories 在查询时召回它，
// extractMemories 则预先注入该列表，避免提取 Agent 浪费一轮执行 `ls`。
//
// 单遍处理：读取每个文件内容时一并获取 mtime，采用先读后排序，而不是 stat、排序、再读取。
// 单个文件的错误会被静默忽略，无法打开一个文件不应导致整个扫描失败。
func ScanMemoryFiles(ctx context.Context, memoryDir string, scope string) ([]MemoryHeader, error) {
	var mdFiles []string
	walkErr := filepath.WalkDir(memoryDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == AutoMemEntrypointName {
			return nil
		}
		mdFiles = append(mdFiles, path)
		return nil
	})
	if walkErr != nil {
		return nil, nil
	}

	results := make([]MemoryHeader, 0, len(mdFiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, filePath := range mdFiles {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			hdr, ok := readMemoryHeader(fp, memoryDir)
			if !ok {
				return
			}
			hdr.Scope = scope
			mu.Lock()
			results = append(results, hdr)
			mu.Unlock()
		}(filePath)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].MtimeMs > results[j].MtimeMs
	})
	if len(results) > MaxMemoryFiles {
		results = results[:MaxMemoryFiles]
	}
	return results, nil
}

func readMemoryHeader(filePath, memoryDir string) (MemoryHeader, bool) {
	info, err := os.Stat(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	f, err := os.Open(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var sb strings.Builder
	for i := 0; i < FrontmatterMaxLines && scanner.Scan(); i++ {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}

	mf := parseFrontmatter(sb.String())
	rel, err := filepath.Rel(memoryDir, filePath)
	if err != nil {
		rel = filepath.Base(filePath)
	}
	return MemoryHeader{
		Filename:    rel,
		FilePath:    filePath,
		MtimeMs:     info.ModTime().UnixMilli(),
		Description: mf.Description,
		Type:        mf.Type,
	}, true
}

// FormatMemoryManifest 把记忆文件头格式化为文本清单：每个文件一行，格式为
// [type] filename (timestamp): description。召回选择器提示词和提取 Agent 提示词都会使用它。
func FormatMemoryManifest(memories []MemoryHeader) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range memories {
		if i > 0 {
			b.WriteByte('\n')
		}
		var tag string
		if m.Type != "" {
			tag = fmt.Sprintf("[%s] ", m.Type)
		}
		var scope string
		if m.Scope != "" {
			scope = fmt.Sprintf("[%s-scope] ", m.Scope)
		}
		ts := time.UnixMilli(m.MtimeMs).UTC().Format("2006-01-02T15:04:05.000Z")
		path := m.FilePath
		if path == "" {
			path = m.Filename
		}
		if m.Description != "" {
			fmt.Fprintf(&b, "- %s%s%s (%s): %s", scope, tag, path, ts, m.Description)
		} else {
			fmt.Fprintf(&b, "- %s%s%s (%s)", scope, tag, path, ts)
		}
	}
	return b.String()
}
