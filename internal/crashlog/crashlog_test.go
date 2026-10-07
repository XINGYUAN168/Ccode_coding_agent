package crashlog

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func readLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".ccode", "crash.log"))
	if err != nil {
		t.Fatalf("read crash log: %v", err)
	}
	return string(data)
}

func TestRecordAppends(t *testing.T) {
	t.Chdir(t.TempDir())

	Record("start pid=1")
	RecordPanic("main", "boom", []byte("goroutine 1 [running]:"))

	log := readLog(t)
	if !strings.Contains(log, "start pid=1") {
		t.Errorf("start record missing: %s", log)
	}
	if !strings.Contains(log, "crash [main] boom") {
		t.Errorf("panic record missing: %s", log)
	}
	if !strings.Contains(log, "goroutine 1 [running]:") {
		t.Errorf("stack missing: %s", log)
	}
	// 追加写：后一条不能把前一条冲掉
	if strings.Index(log, "start pid=1") > strings.Index(log, "crash [main]") {
		t.Error("records are not in append order")
	}
}

func TestInstallWritesStartAndExit(t *testing.T) {
	t.Chdir(t.TempDir())
	// runtime 会持有 crash 输出文件直到进程退出，测试结束前解绑，
	// 否则临时目录清理不掉
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })

	recordExit := Install()
	if got := readLog(t); !strings.Contains(got, "start pid=") {
		t.Errorf("start record missing: %s", got)
	}
	recordExit()

	log := readLog(t)
	if !strings.Contains(log, "exit pid=") {
		t.Errorf("exit record missing: %s", log)
	}
}
