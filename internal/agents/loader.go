package agents

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type AgentLoader struct {
	workDir string
	agents  map[string]*AgentDefinition

	// FailedFiles 记录最近一次 LoadAll 中解析失败的定义文件。每项格式为
	// "<path>: <reason>"。
	FailedFiles []string

	// ErrorWriter 接收解析失败的单行警告，默认写入 os.Stderr；测试会覆盖它以捕获输出。
	ErrorWriter io.Writer
}

func NewAgentLoader(workDir string) *AgentLoader {
	return &AgentLoader{
		workDir:     workDir,
		agents:      make(map[string]*AgentDefinition),
		ErrorWriter: os.Stderr,
	}
}

// getBuiltinSpecs 返回内建的 Agent 定义。验证型 Agent 默认不开，由
// CCODE_VERIFICATION_AGENT 环境变量控制，需要时才加进来。
func getBuiltinSpecs() map[string]SubAgentSpec {
	result := make(map[string]SubAgentSpec, len(BuiltinSpecs)+1)
	for name, spec := range BuiltinSpecs {
		result[name] = spec
	}
	if os.Getenv("CCODE_VERIFICATION_AGENT") == "true" {
		result[VerificationAgentType] = verificationSpec
	}
	return result
}

func (l *AgentLoader) LoadAll() error {
	l.FailedFiles = l.FailedFiles[:0]
	for name, spec := range getBuiltinSpecs() {
		l.agents[name] = &AgentDefinition{
			AgentType:       spec.Name,
			WhenToUse:       spec.Description,
			DisallowedTools: spec.DisallowedTools,
			Model:           spec.Model,
			MaxTurns:        spec.MaxTurns,
			SystemPrompt:    spec.SystemPromptOverride,
			Background:      spec.Background,
			Source:          "built-in",
		}
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		l.loadDir(filepath.Join(home, ".ccode", "agents"), "user")
	}

	if l.workDir != "" {
		l.loadDir(filepath.Join(l.workDir, ".ccode", "agents"), "project")
	}

	return nil
}

func (l *AgentLoader) loadDir(dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		def, err := ParseAgentFile(path)
		if err != nil {
			msg := fmt.Sprintf("%s: %v", path, err)
			l.FailedFiles = append(l.FailedFiles, msg)
			if l.ErrorWriter != nil {
				fmt.Fprintf(l.ErrorWriter, "[ccode] agent definition skipped — %s\n", msg)
			}
			continue
		}
		def.Source = source
		l.agents[def.AgentType] = def
	}
}

func (l *AgentLoader) Get(agentType string) *AgentDefinition {
	return l.agents[agentType]
}

func (l *AgentLoader) ListNames() []string {
	names := make([]string, 0, len(l.agents))
	for name := range l.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
