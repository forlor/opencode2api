package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"opencode2api/proxy"
)

// OpenCode 错误响应结构
type OpenCodeErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseOpenAIRequest 解析客户端原始请求体为通用 map，保留所有参数（temperature/max_tokens 等）
func ParseOpenAIRequest(rawBody []byte) (map[string]interface{}, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = make(map[string]interface{})
	}
	return payload, nil
}

// MarshalOpenAIRequest 将目标模型替换进 payload 并序列化为 JSON body
// 只序列化一次，重试/切换节点时复用同一 body，避免大 payload 重复 marshal
func MarshalOpenAIRequest(payload map[string]interface{}, mappedModel string) ([]byte, error) {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	// 替换为目标模型，保留其余字段原样透传
	payload["model"] = mappedModel
	return json.Marshal(payload)
}

// BuildOpenCodeHTTPRequest 将已序列化的 body 转化为符合 OpenCode 规范的 http.Request
// apiPath 为客户端入口路径（如 /v1/chat/completions、/v1/messages），转发到节点相同路径
// ctx 传入客户端请求的 context，客户端断开后上游请求会被同步取消
func BuildOpenCodeHTTPRequest(ctx context.Context, node *proxy.Node, bodyBytes []byte, apiPath, secret string) (*http.Request, error) {
	url := node.LANURL + "/zen" + apiPath
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	// 生成随机 Request ID: msg_ + 26字符
	reqID := generateRequestID()

	// 伪造符合标准的 Headers
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/local ai-sdk/provider-utils/4.0.23 runtime/node.js/24")
	req.Header.Set("X-Opencode-Client", "desktop")
	req.Header.Set("X-Opencode-Project", "global")
	req.Header.Set("X-Opencode-Request", reqID)
	req.Header.Set("X-Opencode-Session", node.GetSessionID()) // 使用节点绑定的 Session
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", "*")
	// 不设置 Accept-Encoding，交由 Go http.Client 自动处理 gzip 解压 (Go 不自动解压 br)
	req.Header.Set("Sec-Fetch-Mode", "cors")

	// 局域网 Nginx 内部安全密钥 Header
	if secret != "" {
		req.Header.Set("X-Proxy-Secret", secret)
	}

	return req, nil
}

// 请求 ID 生成：时间戳 + 原子计数器组合，避免每次请求的 crypto/rand 系统调用开销
var reqIDCounter uint64

func generateRequestID() string {
	ts := time.Now().UnixNano()
	seq := atomic.AddUint64(&reqIDCounter, 1)
	return fmt.Sprintf("msg_%x_%x", ts, seq)
}

// CheckIsFreeUsageLimitError 检查 Body 或 JSON 结构中是否包含 FreeUsageLimitError 报错
func CheckIsFreeUsageLimitError(bodyBytes []byte) bool {
	var errResp OpenCodeErrorResponse
	if err := json.Unmarshal(bodyBytes, &errResp); err == nil {
		if errResp.Error.Type == "FreeUsageLimitError" || errResp.Type == "FreeUsageLimitError" {
			return true
		}
	}
	// 包含关键字备用检查
	return bytes.Contains(bodyBytes, []byte("FreeUsageLimitError")) || bytes.Contains(bodyBytes, []byte("Rate limit exceeded"))
}

// EmptyCompletionInfo 从上游"空完成"响应中提取的可读诊断信息
type EmptyCompletionInfo struct {
	ID    string
	Model string
}

// ParseEmptyCompletion 判断上游响应体是否为"空完成"响应：
// OpenAI chat.completion 结构但 message 无 content（缺失/空/null）且 finish_reason 为 null/空。
// 通常代表上游模型未产出任何内容（免费额度受限/上下文过长/请求参数不受支持等）。
// 命中时返回 true 与可从响应体提取的 id/model，便于生成可读错误信息。
func ParseEmptyCompletion(body []byte) (bool, EmptyCompletionInfo) {
	var m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false, EmptyCompletionInfo{}
	}
	if m.Object != "chat.completion" && m.Object != "chat.completion.chunk" {
		return false, EmptyCompletionInfo{}
	}
	if len(m.Choices) == 0 {
		return false, EmptyCompletionInfo{}
	}
	// 只判断首个 choice：错误响应通常只有一条
	c := m.Choices[0]
	if !isEmptyContent(c.Message.Content) || !isEmptyFinish(c.FinishReason) {
		return false, EmptyCompletionInfo{}
	}
	return true, EmptyCompletionInfo{ID: m.ID, Model: m.Model}
}

// isEmptyContent 判断 content 是否为空：nil / 空白字符串 / 空内容块数组
func isEmptyContent(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	}
	return false
}

// isEmptyFinish 判断 finish_reason 是否为空：null 或空字符串
func isEmptyFinish(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return s == ""
	}
	return false
}

// BuildEmptyCompletionError 将"空完成"响应改造成带可读诊断信息的 OpenAI error 响应体。
// 保留原始响应（截断）便于排查上游真实原因；状态码沿用上游状态码。
func BuildEmptyCompletionError(info EmptyCompletionInfo, original []byte, nodeName string, statusCode int) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "上游节点 %s 返回空完成响应 (HTTP %d)：模型未产出任何内容，可能原因：免费额度受限、上下文过长或请求参数不受该模型支持。", nodeName, statusCode)
	if info.Model != "" {
		fmt.Fprintf(&sb, " 上游模型: %s。", info.Model)
	}
	if info.ID != "" {
		fmt.Fprintf(&sb, " 请求ID: %s。", info.ID)
	}
	if len(original) > 0 {
		fmt.Fprintf(&sb, " 原始响应: %s", truncateBytes(original))
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_empty_response",
			"message": sb.String(),
			"code":    statusCode,
		},
	})
	return b
}

// truncateBytes 截断原始响应体，避免错误信息过长刷屏
func truncateBytes(b []byte) string {
	const maxLen = 500
	if len(b) > maxLen {
		return string(b[:maxLen]) + "... (truncated)"
	}
	return string(b)
}
