package memory

import "ccode/internal/permissions"

// NewSubAgentChecker 组装后台记忆 Agent（提取与整理）的权限检查器。
//
// 沙箱基线取项目根，与主 Agent 保持一致；用户级记忆目录在项目根之外，
// 后台 Agent 要往里写 user / feedback 类记忆，作为额外允许路径显式带上。
// 规则引擎读项目内那三份规则文件，后台执行同样受用户配置的 deny/ask 约束。
func NewSubAgentChecker(projectRoot, userMemoryDir string) *permissions.Checker {
	var extraRoots []string
	if userMemoryDir != "" {
		extraRoots = append(extraRoots, userMemoryDir)
	}
	return permissions.NewChecker(
		permissions.NewPathSandbox(projectRoot, extraRoots...),
		permissions.NewRuleEngine(projectRoot),
		permissions.ModeBypass,
	)
}
