package llm

// ModelInfo 只包含可安全展示或参与上下文预算的模型元数据。
type ModelInfo struct {
	ID            string
	DisplayName   string
	Provider      string
	ContextWindow int
}
