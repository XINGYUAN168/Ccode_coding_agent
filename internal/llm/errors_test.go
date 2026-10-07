package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   ErrorKind
	}{
		{401, "", ErrorAuth},
		{403, "", ErrorAuth},
		{404, "", ErrorModel},
		{429, "", ErrorRateLimit},
		{413, "", ErrorContext},
		{400, "context_length_exceeded", ErrorContext},
		{422, "bad request", ErrorProtocol},
		{500, "", ErrorNetwork},
		{418, "", ErrorUnknown},
	}
	for _, tt := range tests {
		if got := HTTPStatus(tt.status, tt.body, nil).Kind; got != tt.want {
			t.Errorf("status %d kind = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestClassifyContextAndCause(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", context.Canceled)
	got := Classify(err)
	if got.Kind != ErrorCanceled || !errors.Is(got, context.Canceled) {
		t.Fatalf("取消分类或 Cause 丢失: %#v", got)
	}
}

func TestErrorNeverLeaksCanary(t *testing.T) {
	canary := "sk-canary-SECRET-Error-123456789"
	cause := errors.New("POST https://secret.example/v1 Authorization: Bearer " + canary)
	err := NewError(ErrorNetwork, "请求失败 "+cause.Error(), cause)
	for _, rendered := range []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%v", err)} {
		if strings.Contains(rendered, canary) || strings.Contains(rendered, "secret.example") {
			t.Fatalf("错误泄漏敏感内容: %s", rendered)
		}
	}
	if !errors.Is(err, cause) {
		t.Fatal("Cause 应可供 errors.Is 使用")
	}
}

func TestRetryAfterSanitized(t *testing.T) {
	if got := NewRateLimitError("15", nil).RetryAfter; got != "15" {
		t.Fatalf("合法 Retry-After = %q", got)
	}
	if got := NewRateLimitError("secret-value", nil).RetryAfter; got != "" {
		t.Fatalf("非法 Retry-After 未清除: %q", got)
	}
}
