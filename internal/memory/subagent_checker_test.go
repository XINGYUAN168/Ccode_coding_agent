package memory

import (
	"os"
	"path/filepath"
	"testing"

	"ccode/internal/permissions"
)

func TestNewSubAgentCheckerOpensUserMemoryDir(t *testing.T) {
	// 沙箱默认放行系统临时目录，取 home 下的路径才能检验放开的是不是这一条
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无法解析 home 目录")
	}
	projectRoot := t.TempDir()
	userMemoryDir := filepath.Join(home, ".ccode", "memory")

	chk := NewSubAgentChecker(projectRoot, userMemoryDir)

	// 用户级记忆目录在项目根之外，后台 Agent 要往里写，应当被放开
	if ok, reason := chk.Sandbox.Check(filepath.Join(userMemoryDir, "MEMORY.md")); !ok {
		t.Errorf("用户级记忆目录应被放开，却被拦下: %s", reason)
	}

	// 项目内路径照常放行
	if ok, reason := chk.Sandbox.Check(filepath.Join(projectRoot, "a.txt")); !ok {
		t.Errorf("项目内路径应放行，却被拦下: %s", reason)
	}

	// home 下的无关目录不受影响，仍然被挡住
	unrelated := filepath.Join(home, "unrelated-dir-for-ccode-test", "x.txt")
	if ok, _ := chk.Sandbox.Check(unrelated); ok {
		t.Error("无关的项目外目录不应被放开")
	}
}

func TestNewSubAgentCheckerCarriesProjectRules(t *testing.T) {
	projectRoot := t.TempDir()

	chk := NewSubAgentChecker(projectRoot, "")

	// 规则引擎读的是项目内那三份规则文件，后台执行同样受用户配置约束
	if want := filepath.Join(projectRoot, ".ccode", "permissions.yaml"); chk.RuleEngine.ProjectPath != want {
		t.Errorf("ProjectPath = %q, want %q", chk.RuleEngine.ProjectPath, want)
	}
	if want := filepath.Join(projectRoot, ".ccode", "permissions.local.yaml"); chk.RuleEngine.LocalPath != want {
		t.Errorf("LocalPath = %q, want %q", chk.RuleEngine.LocalPath, want)
	}

	// 后台 Agent 无人应答，只能跑在 bypass 模式
	if chk.Mode != permissions.ModeBypass {
		t.Errorf("Mode = %q, want bypassPermissions", chk.Mode)
	}
}
