package llm

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// ErrorKind 是上层可以稳定判断的模型错误类别。
type ErrorKind string

const (
	ErrorAuth      ErrorKind = "auth"
	ErrorRateLimit ErrorKind = "rate_limit"
	ErrorModel     ErrorKind = "model"
	ErrorContext   ErrorKind = "context"
	ErrorNetwork   ErrorKind = "network"
	ErrorProtocol  ErrorKind = "protocol"
	ErrorCanceled  ErrorKind = "canceled"
	ErrorUnknown   ErrorKind = "unknown"
)

// Error 保存安全展示文案和仅供 errors.Is/As 使用的底层原因。
// Error 方法不会拼接 Cause，避免把请求地址、响应正文或认证材料暴露给用户。
type Error struct {
	Kind       ErrorKind
	Message    string
	RetryAfter string
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return redact(e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewError 构造已分类错误，并在保存前对展示文案做纵深脱敏。
func NewError(kind ErrorKind, message string, cause error) *Error {
	return &Error{Kind: kind, Message: redact(strings.TrimSpace(message)), Cause: cause}
}

// NewRateLimitError 构造携带安全 Retry-After 值的限流错误。
func NewRateLimitError(retryAfter string, cause error) *Error {
	retryAfter = sanitizeRetryAfter(retryAfter)
	return &Error{
		Kind:       ErrorRateLimit,
		Message:    defaultMessage(ErrorRateLimit),
		RetryAfter: retryAfter,
		Cause:      cause,
	}
}

// Classify 将任意错误转换为统一错误；已分类错误会保留类别和重试信息。
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var modelErr *Error
	if errors.As(err, &modelErr) {
		copy := *modelErr
		copy.Message = redact(copy.Message)
		copy.RetryAfter = sanitizeRetryAfter(copy.RetryAfter)
		return &copy
	}
	switch {
	case errors.Is(err, context.Canceled):
		return NewError(ErrorCanceled, defaultMessage(ErrorCanceled), err)
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(ErrorNetwork, "请求超时，请检查网络与 base_url 后重试", err)
	default:
		return NewError(ErrorUnknown, defaultMessage(ErrorUnknown), err)
	}
}

// HTTPStatus 根据 HTTP 状态和有限的响应信号分类错误。响应正文只参与上下文过长
// 判断，绝不会进入展示消息。
func HTTPStatus(status int, body string, cause error, retryAfter ...string) *Error {
	lower := strings.ToLower(body)
	if status == 413 || strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "context window") || strings.Contains(lower, "prompt is too long") {
		return NewError(ErrorContext, defaultMessage(ErrorContext), cause)
	}
	if status == 429 {
		retry := ""
		if len(retryAfter) > 0 {
			retry = retryAfter[0]
		}
		return NewRateLimitError(retry, cause)
	}
	kind := ErrorUnknown
	switch {
	case status == 401 || status == 403:
		kind = ErrorAuth
	case status == 404:
		kind = ErrorModel
	case status == 400 || status == 422:
		kind = ErrorProtocol
	case status == 408 || status >= 500:
		kind = ErrorNetwork
	}
	return NewError(kind, defaultMessage(kind), cause)
}

func Protocol(cause error) *Error {
	return NewError(ErrorProtocol, defaultMessage(ErrorProtocol), cause)
}

func Network(cause error) *Error {
	return NewError(ErrorNetwork, defaultMessage(ErrorNetwork), cause)
}

func defaultMessage(kind ErrorKind) string {
	switch kind {
	case ErrorAuth:
		return "认证失败，请检查配置中的 api_key"
	case ErrorRateLimit:
		return "请求被限流，请稍后重试"
	case ErrorModel:
		return "模型不可用，请检查配置中的 model"
	case ErrorContext:
		return "上下文长度超过模型限制"
	case ErrorNetwork:
		return "网络请求失败，请检查连接与 base_url"
	case ErrorProtocol:
		return "服务端响应不符合预期协议"
	case ErrorCanceled:
		return "请求已取消"
	default:
		return "模型请求失败"
	}
}

var (
	secretHeader = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|api-key|api_key)\b\s*[:=]\s*\S+`)
	bearerToken  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-~+/=]+`)
	skToken      = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{6,}\b`)
	urlToken     = regexp.MustCompile(`https?://\S+`)
	longToken    = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
	retryAfterRE = regexp.MustCompile(`^[0-9]{1,9}$`)
)

func redact(s string) string {
	s = secretHeader.ReplaceAllString(s, "$1: <redacted>")
	s = bearerToken.ReplaceAllString(s, "Bearer <redacted>")
	s = skToken.ReplaceAllString(s, "<redacted>")
	s = urlToken.ReplaceAllString(s, "<redacted-url>")
	s = longToken.ReplaceAllString(s, "<redacted>")
	return s
}

func sanitizeRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if retryAfterRE.MatchString(value) {
		return value
	}
	return ""
}
