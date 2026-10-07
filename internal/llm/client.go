package llm

import (
	"context"

	"ccode/internal/conversation"
)

type Client interface {
	Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan StreamEvent, <-chan error)
	SetSystemPrompt(prompt string)
}

type MaxTokensSetter interface {
	SetMaxOutputTokens(tokens int)
}
