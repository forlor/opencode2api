package adapter

import (
	"bytes"
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

// BuildOpenCodeHTTPRequest 将 OpenAI 请求转化为符合 OpenCode 规范的 http.Request
// 接收已解析的 payload map，仅替换 model 字段，其余参数原样透传
func BuildOpenCodeHTTPRequest(node *proxy.Node, payload map[string]interface{}, mappedModel, secret string) (*http.Request, error) {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	// 替换为目标模型，保留其余字段原样透传
	payload["model"] = mappedModel

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := node.LANURL + "/zen/v1/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
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
	req.Header.Set("Accept-Encoding", "br, gzip, deflate")
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
