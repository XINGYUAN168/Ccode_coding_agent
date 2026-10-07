// Package crashlog 记录进程的启动、退出和崩溃现场，供异常退出后追查。
package crashlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const logDir = ".ccode"

func logPath() string {
	return filepath.Join(logDir, "crash.log")
}

// Record 往崩溃日志追加一行带时间戳的记录。
// 诊断本身不能反过来把进程搞挂，所以写失败一律静默跳过。
func Record(text string) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format(time.RFC3339), text)
}

// RecordPanic 记录一次 panic，带完整调用栈。context 用来区分现场来自哪一层。
func RecordPanic(context string, value any, stack []byte) {
	Record(fmt.Sprintf("crash [%s] %v\n%s", context, value, stack))
}

// Install 安装崩溃诊断，进程启动时调用一次，返回的函数在退出前调用以留下 exit 标记。
//
// 留下三类痕迹：start 行标记本次运行开始；exit 行在 main 正常返回时写出；
// SetCrashOutput 接管 runtime 层崩溃的输出，goroutine 里的 panic、并发写 map、
// 死锁检测这类 fatal error 不经过 recover，只有重定向到文件才能在事后看到现场。
// 三者组合即可判定退出方式：有 crash 有 exit 是崩溃退出，只有 start 和 exit 是
// 正常退出，只有 start 说明进程是被外部结束的，自身来不及留下任何痕迹。
func Install() func() {
	Record(fmt.Sprintf("start pid=%d", os.Getpid()))
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			// SetCrashOutput 会自己复制一份文件描述符，这里随即关闭即可，
			// 不必长期占着文件句柄
			_ = debug.SetCrashOutput(f, debug.CrashOptions{})
			_ = f.Close()
		}
	}
	return func() {
		Record(fmt.Sprintf("exit pid=%d", os.Getpid()))
	}
}
