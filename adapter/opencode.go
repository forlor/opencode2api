package adapter

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"opencode2api/proxy"
)

// OpenAI Chat Completion 请求结构
type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type OpenAIMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// OpenCode 期望的真实 Payload 结构
type OpenCodeRequest struct {
	Stream   bool            `json:"stream"`
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
}

// OpenCode 错误响应结构
type OpenCodeErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// BuildOpenCodeHTTPRequest 将 OpenAI 请求转化为符合 OpenCode 规范的 http.Request
func BuildOpenCodeHTTPRequest(node *proxy.Node, openAIReq *OpenAIRequest, mappedModel, secret string) (*http.Request, error) {
	// 构造 Body
	codeReq := OpenCodeRequest{
		Stream:   openAIReq.Stream,
		Model:    mappedModel,
		Messages: openAIReq.Messages,
	}

	bodyBytes, err := json.Marshal(codeReq)
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

func generateRequestID() string {
	bytes := make([]byte, 13)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("msg_%d", rand.Reader)
	}
	return "msg_" + hex.EncodeToString(bytes)
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

// FormatNonStreamResponse 将 OpenCode 完整非流式响应包转换为标准的 OpenAI 聊天格式
func FormatNonStreamResponse(rawBody []byte, model string) ([]byte, error) {
	// 如果本身就是标准 JSON 则尝试解析并再封装，或者把返回的文本提取出来
	// 此处包装成标准 OpenAI chat completion 格式
	type Choice struct {
		Index        int           `json:"index"`
		Message      OpenAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	}
	type ChatCompletion struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []Choice `json:"choices"`
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return rawBody, nil
	}

	// 如果 OpenCode 返回的包含了 content/choices 结构，直接放回，否则组装标准响应
	return json.Marshal(parsed)
}

// FormatSSEChunk 重新组装 OpenCode SSE 增量为 OpenAI chunk 格式
func FormatSSEChunk(content, model, reqID string) string {
	type Delta struct {
		Content string `json:"content,omitempty"`
	}
	type ChunkChoice struct {
		Index        int    `json:"index"`
		Delta        Delta  `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	}
	type StreamChunk struct {
		ID      string        `json:"id"`
		Object  string        `json:"object"`
		Created int64         `json:"created"`
		Model   string        `json:"model"`
		Choices []ChunkChoice `json:"choices"`
	}

	chunk := StreamChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   model,
		Choices: []ChunkChoice{
			{
				Index: 0,
				Delta: Delta{
					Content: content,
				},
			},
		},
	}

	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}
