package memory

import (
	"fmt"
	"time"
)

// MemoryAgeDays 返回从 mtime 至今经过的天数并向下取整：今天为 0，昨天为 1，
// 更早为 2 以上。未来时间或时钟偏差产生的负值会收敛到 0。
func MemoryAgeDays(mtimeMs int64) int {
	d := (time.Now().UnixMilli() - mtimeMs) / 86_400_000
	if d < 0 {
		return 0
	}
	return int(d)
}

// MemoryAge 返回便于理解的时间间隔文本。模型不擅长日期运算；原始 ISO 时间戳
// 不像“47 天前”那样容易触发过期判断。
func MemoryAge(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d == 0 {
		return "today"
	}
	if d == 1 {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", d)
}

// MemoryFreshnessText 为超过 1 天的记忆返回纯文本过期提示。对今天或昨天的新鲜记忆
// 返回 ""，因为此时提示只会形成噪声。
//
// 当使用方已经自行提供外层包装时使用此函数，例如
// 调用链为 messages relevant_memories → wrapMessagesInSystemReminder）。
//
// 这样设计是因为用户曾反馈过时代码状态记忆仍被当作事实，例如引用已变更代码的
// file:line。引用会让过时结论显得更权威，而不是降低可信度。
func MemoryFreshnessText(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d <= 1 {
		return ""
	}
	return fmt.Sprintf(
		"This memory is %d days old. "+
			"Memories are point-in-time observations, not live state — "+
			"claims about code behavior or file:line citations may be outdated. "+
			"Verify against current code before asserting as fact.",
		d,
	)
}

// MemoryFreshnessNote 返回用 <system-reminder> 标签包裹的单条记忆过期说明。
// 记忆不超过 1 天时返回 ""。调用方未自行添加 system-reminder 包装时使用此函数。
func MemoryFreshnessNote(mtimeMs int64) string {
	text := MemoryFreshnessText(mtimeMs)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<system-reminder>%s</system-reminder>\n", text)
}
