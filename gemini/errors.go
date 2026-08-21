package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ErrKind Gemini 上游错误的业务分类，用于决定 key 的处理策略
type ErrKind int

const (
	ErrNone         ErrKind = iota // 健康流/无错误
	ErrRateLimit                   // 429 / RESOURCE_EXHAUSTED → 换 key + 冷却
	ErrInvalidKey                  // 403 / API key not valid → ban 该 key
	ErrTemporary                   // 5xx / UNAVAILABLE → 指数退避重试
	ErrDeterministic               // 400/404/422 等 → 直接透传给客户端，不烧 key
)

func (k ErrKind) String() string {
	switch k {
	case ErrNone:
		return "none"
	case ErrRateLimit:
		return "rate_limit"
	case ErrInvalidKey:
		return "invalid_key"
	case ErrTemporary:
		return "temporary"
	case ErrDeterministic:
		return "deterministic"
	default:
		return "unknown"
	}
}

// IsTemporaryStatus 是否属于可退避重试的 5xx 服务端临时错误
func IsTemporaryStatus(status int) bool {
	return status == http.StatusServiceUnavailable ||
		status == http.StatusBadGateway ||
		status == http.StatusInternalServerError ||
		status == http.StatusGatewayTimeout
}

// ParseGeminiError 从响应体解析顶层 error 对象
func ParseGeminiError(body []byte) (*GeminiError, bool) {
	var wrapper struct {
		Error *GeminiError `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, false
	}
	if wrapper.Error == nil || wrapper.Error.Status == "" && wrapper.Error.Message == "" {
		return nil, false
	}
	return wrapper.Error, true
}

// ClassifyGeminiHttp 结合 HTTP 状态码与响应体做最终分类
func ClassifyGeminiHttp(status int, body []byte) ErrKind {
	if gerr, ok := ParseGeminiError(body); ok {
		return ClassifyErrorStatus(gerr.Status, gerr.Message)
	}
	// 无结构化 error 时按裸状态码分类
	if status == http.StatusTooManyRequests {
		return ErrRateLimit
	}
	if status == http.StatusForbidden {
		return ErrInvalidKey
	}
	if IsTemporaryStatus(status) {
		return ErrTemporary
	}
	if status >= 400 && status < 500 {
		return ErrDeterministic
	}
	return ErrNone
}

// ClassifyErrorStatus 按 Gemini error.status / message 分类
func ClassifyErrorStatus(status, message string) ErrKind {
	s := strings.ToUpper(strings.TrimSpace(status))
	switch {
	case s == "RESOURCE_EXHAUSTED" || s == "RATE_LIMIT_EXCEEDED":
		return ErrRateLimit
	case s == "PERMISSION_DENIED" || s == "UNAUTHENTICATED":
		return ErrInvalidKey
	case s == "UNAVAILABLE" || s == "DEADLINE_EXCEEDED":
		return ErrTemporary
	case s == "INVALID_ARGUMENT" || s == "NOT_FOUND" || s == "FAILED_PRECONDITION" ||
		s == "ALREADY_EXISTS" || s == "OUT_OF_RANGE" || s == "UNIMPLEMENTED":
		return ErrDeterministic
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "api key not valid") || strings.Contains(lower, "invalid api key") {
		return ErrInvalidKey
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "resource exhausted") {
		return ErrRateLimit
	}
	if status != "" {
		return ErrDeterministic
	}
	return ErrTemporary
}

// ReadAndCheckGeminiStreamError 检查流/响应的开头是否立即返回了错误。
// 返回：
//   - errBody：非 200 时的完整 body，或 200-SSE 首帧错误事件的 data JSON（供透传/记录）
//   - kind：ErrRateLimit/ErrInvalidKey/ErrTemporary 表示错误，ErrNone 表示健康
//   - err：读取失败
//
// 200 健康流不会被消费：已扫描字节用 io.MultiReader 精确回填到 resp.Body，保证后续透传不丢不重。
func ReadAndCheckGeminiStreamError(resp *http.Response) ([]byte, ErrKind, error) {
	// 非 200：直接返回 JSON body
	if resp.StatusCode != http.StatusOK {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, ErrNone, err
		}
		return bodyBytes, ClassifyGeminiHttp(resp.StatusCode, bodyBytes), nil
	}

	// 200：SSE 可能把错误包在第一个 data: 事件里（free 额度 429 常见形态）
	const (
		maxScanBytes = 8 * 1024
		maxScanLines = 8
	)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var scanned []byte
	scanLines := 0

	for scanLines < maxScanLines && len(scanned) <= maxScanBytes {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			line = append(append([]byte(nil), line...), readGeminiRemaining(reader)...)
			err = nil
		}
		scanned = append(scanned, line...)
		scanLines++

		// data: 事件行 → 检查是否 error
		if data := extractDataJSON(scanned); len(data) > 0 {
			if gerr, ok := ParseGeminiError(data); ok {
				kind := ClassifyErrorStatus(gerr.Status, gerr.Message)
				return data, kind, nil
			}
		}
		if err != nil {
			break
		}
	}

	// 健康流：把扫描过的字节拼回，后续透传完整无重复
	oldBody := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(scanned), reader),
		Closer: oldBody,
	}
	return nil, ErrNone, nil
}

// extractDataJSON 从已扫描字节里提取最后一个 `data:` 行的 JSON（去除前缀与尾部换行）
func extractDataJSON(scanned []byte) []byte {
	trimmed := bytes.TrimSpace(scanned)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	// 可能多行扫描，逐行找 data:
	lines := bytes.Split(scanned, []byte("\n"))
	var lastData []byte
	for _, line := range lines {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte("data:")) {
			raw := bytes.TrimSpace(t[len("data:"):])
			lastData = raw
		}
	}
	if len(lastData) == 0 {
		return nil
	}
	// 去除可能的多余逗号/空行不处理，直接尝试解析
	var probe map[string]any
	if err := json.Unmarshal(lastData, &probe); err != nil {
		return nil
	}
	return lastData
}

// readGeminiRemaining 读取到换行为止的剩余内容（配合 ReadSlice 的 ErrBufferFull）
func readGeminiRemaining(reader *bufio.Reader) []byte {
	var tail []byte
	for {
		part, err := reader.ReadSlice('\n')
		tail = append(tail, part...)
		if err == nil || err == io.EOF {
			return tail
		}
		if err != bufio.ErrBufferFull {
			return tail
		}
	}
}