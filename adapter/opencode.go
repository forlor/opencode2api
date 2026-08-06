package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
