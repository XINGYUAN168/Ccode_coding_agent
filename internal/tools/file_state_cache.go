package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStateCache 跟踪已读取文件及其修改时间，通过“先读后改”约束防止盲目覆盖。
type FileStateCache struct {
	mu      sync.Mutex
	entries map[string]int64 // 路径 → 修改时间（UnixMilli）
}

func NewFileStateCache() *FileStateCache {
	return &FileStateCache{
		entries: make(map[string]int64),
	}
}

// Record 在成功读取后保存文件修改时间。
func (c *FileStateCache) Record(filePath string, mtime int64) {
	abs := normalizePath(filePath)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[abs] = mtime
}

// Check 验证文件已经读取且此后未被修改。可以编辑时返回 (true, "")；
// 应阻止编辑时返回 (false, errorMessage)。
func (c *FileStateCache) Check(filePath string) (bool, string) {
	abs := normalizePath(filePath)
	c.mu.Lock()
	cachedMtime, exists := c.entries[abs]
	c.mu.Unlock()

	if !exists {
		return false, fmt.Sprintf("Error: file has not been read yet. Read it first before editing.")
	}

	info, err := os.Stat(abs)
	if err != nil {
		// 文件可能已被删除，交由调用方处理。
		return true, ""
	}
	currentMtime := info.ModTime().UnixMilli()
	if currentMtime > cachedMtime {
		return false, fmt.Sprintf("Error: file has been modified since last read. Read it again before editing.")
	}

	return true, ""
}

// Update 在成功编辑或写入后刷新缓存项。
func (c *FileStateCache) Update(filePath string) {
	abs := normalizePath(filePath)
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[abs] = info.ModTime().UnixMilli()
}

func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
