package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// gitNoPromptEnv 返回此包启动的每个 Git 子进程所用的基础环境，并追加两个安全开关：
//
// GIT_TERMINAL_PROMPT=0：阻止 Git 打开 /dev/tty 请求凭证（否则会导致 CLI 卡住）。
// GIT_ASKPASS=""：禁用 askpass 图形程序（通过另一条代码路径达到相同效果）。
//
// 配合 *exec.Cmd 上的 Stdin = nil，这会关闭 Git 因等待交互输入而阻塞的所有通道。
func gitNoPromptEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
}

// runGit 在 dir 内以关闭 stdin、禁用提示的环境运行 `git <args.>`。它返回 stdout、
// stderr 和退出码（进程未运行时为 -1）。非零退出不会触发异常；调用方根据上下文决定
// code != 0 是否为错误。
//
// ctx 会传递取消信号：取消 ctx 将终止 Git 子进程。
func runGit(ctx context.Context, dir string, args ...string) (stdout, stderr string, code int) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitNoPromptEnv()
	cmd.Stdin = nil
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if err == nil {
		return stdout, stderr, 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, ee.ExitCode()
	}
	// 进程启动失败（PATH 中没有 Git、目录不存在等）。
	return stdout, stderr, -1
}
