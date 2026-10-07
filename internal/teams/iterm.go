package teams

import (
	"fmt"
	"os/exec"
	"strings"
)

// ModeITerm 通过 AppleScript 在独立的 iTerm2 标签页中启动每个队员。它仅支持
// macOS；设置 ITERM_SESSION_ID 后，detectBackend 会选择此模式。
const ModeITerm TeamMode = "iterm"

// spawnITermTeammate 打开新的 iTerm2 标签页并在其中运行 cliCommand。它返回脚本端的
// 标签页标识（"team-member"），使调用方后续可以准确关闭该标签页。
func spawnITermTeammate(teamName, memberName, cliCommand string) (string, error) {
	tabName := fmt.Sprintf("%s-%s", teamName, memberName)
	// 转义所有内嵌双引号，使 AppleScript 字符串字面量保持有效。
	safeCmd := strings.ReplaceAll(cliCommand, `"`, `\"`)
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  tell current window
    set newTab to create tab with default profile
    tell newTab
      set name to "%s"
      tell current session
        write text "%s"
      end tell
    end tell
  end tell
end tell`, safeName, safeCmd)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osascript: %s: %s", err, strings.TrimSpace(string(out)))
	}
	return tabName, nil
}

// stopITermTeammate 关闭由 spawnITermTeammate 创建的 iTerm2 标签页。该操作尽力而为：
// 标签页不存在或窗口已关闭时不报告错误。
func stopITermTeammate(tabName string) {
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  repeat with w in windows
    repeat with t in tabs of w
      if name of t is "%s" then
        tell t to close
      end if
    end repeat
  end repeat
end tell`, safeName)
	exec.Command("osascript", "-e", script).Run()
}
