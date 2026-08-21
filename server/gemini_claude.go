package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"opencode2api/config"
	"opencode2api/gemini"
)

// handleGeminiClaudeChat Anthropic /v1/messages → Gemini 线路。
// 与 handleGeminiChat 共享 geminiExec 的重试编排（换 key/换节点/错误分类）。
func (r *Router) handleGeminiClaudeChat(w http.ResponseWriter, req *http.Request, payload map[string]any, decision config.RouteDecision, bodyLen int) {
	gcfg := r.cfg.Gemini
	isStream, _ := payload["stream"].(bool)

	// Anthropic 协议规范会校验 max_tokens 必填；Gemini 缺 maxOutputTokens 也会 400 → 兜底
	if payload["max_tokens"] == nil {
		payload["max_tokens"] = gcfg.DefaultMaxOutputTokens
	}
	payload["model"] = decision.TargetModel

	opts := gemini.ConvertOpts{
		ForceThinking:      gcfg.ForceThinking,
		ForceWebSearch:     gcfg.ForceWebSearch,
		ForceCodeExecution: gcfg.ForceCodeExecution,
		ForceUrlContext:    gcfg.ForceUrlContext,
		SafetyThreshold:    gcfg.SafetySettingsThreshold,
	}
	g, suffix, err := gemini.TranslateClaudeToGemini(payload, opts)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	gBody, err := json.Marshal(g)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "internal_error", "序列化 Gemini 请求失败: "+err.Error())
		return
	}
	logOutboundStats(req.URL.Path, decision.TargetModel, bodyLen, len(gBody))

	modelPath := sanitizeGeminiModel(suffix.CleanModel)
	if modelPath == "" {
		writeProxyError(w, http.StatusBadRequest, "invalid_request_error", "非法模型名: "+suffix.CleanModel)
		return
	}

	// 流式模式：模型后缀 > 线级配置 > 默认 real
	mode := decision.Streaming
	if suffix.HasStreamingSuffix {
		mode = suffix.StreamingMode
	}
	realStream := isStream && mode != "fake"
	endpoint := ":generateContent"
	if realStream {
		endpoint = ":streamGenerateContent?alt=sse"
	}

	r.geminiExec(w, req, gBody, modelPath, endpoint, func(w http.ResponseWriter, req *http.Request, node *gemini.GeminiNode, key *gemini.GeminiKey, resp *http.Response) {
		if realStream {
			// 流式"提交点"：自此开始向客户端发出内容，中途错误不再换 key 重放
			forwardGeminiClaudeStream(w, req, resp, decision.TargetModel)
			node.ReleaseKey(key)
			return
		}
		// 非流 / fake 流：全缓冲模式，任意错误都可在到达客户端前安全重试
		respBytes, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		node.ReleaseKey(key)
		if rerr != nil {
			writeProxyError(w, http.StatusBadGateway, "proxy_error", "读取上游响应失败")
			return
		}
		var gr gemini.GeminiResponse
		if err := json.Unmarshal(respBytes, &gr); err != nil {
			writeProxyError(w, http.StatusBadGateway, "proxy_error", "解析 Gemini 响应失败: "+err.Error())
			return
		}
		out := gemini.TranslateGeminiToClaude(&gr, decision.TargetModel)
		if isStream {
			writeClaudeFakeStream(w, out)
		} else {
			writeJSONResponse(w, out)
		}
	})
}

// forwardGeminiClaudeStream 把 Gemini SSE 流实时转换为 Anthropic message SSE 流。
// Gemini 的 message_stop 帧由转换器内置在 finish 事件里，无需追加 [DONE]。
// 中途上游 error → 发 Anthropic error 帧后结束，不重放（提交点语义）。
func forwardGeminiClaudeStream(w http.ResponseWriter, req *http.Request, resp *http.Response, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[gemini] ResponseWriter 不支持 Flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	idleTimer := time.AfterFunc(streamIdleTimeout, func() { resp.Body.Close() })
	defer idleTimer.Stop()

	reader := bufio.NewReader(resp.Body)
	st := &gemini.ClaudeStreamState{}
	var pending []byte
	hasPending := false

	flushEvent := func() {
		if !hasPending {
			return
		}
		raw := bytes.TrimSpace(pending)
		pending = nil
		hasPending = false
		if len(raw) == 0 {
			return
		}
		// 上游中途 error 事件 → 发 Anthropic error 帧并终止
		if gerr, ok := gemini.ParseGeminiError(raw); ok {
			log.Printf("[gemini] 流中途上游错误: %s %s", gerr.Status, truncateLog([]byte(gerr.Message)))
			msg := fmt.Sprintf("[ProxySystem Error] %s %s", gerr.Status, gerr.Message)
			_, _ = w.Write([]byte("event: error\n"))
			_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"proxy_error","message":` + string(mustMarshal(msg)) + `}}` + "\n\n"))
			flusher.Flush()
			return
		}
		out, _ := gemini.TranslateGeminiToClaudeStream(raw, model, st)
		if len(out) > 0 {
			if _, werr := w.Write(out); werr != nil {
				return
			}
			flusher.Flush()
		}
	}

	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			line = append(append([]byte(nil), line...), readLineRemaining(reader)...)
			err = nil
		}
		if len(line) > 0 {
			idleTimer.Reset(streamIdleTimeout)
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				pending = append(pending, []byte(strings.TrimSpace(trimmed[len("data:"):]))...)
				hasPending = true
			} else if trimmed == "" {
				flushEvent()
			}
		}
		if err != nil {
			break
		}
	}
	flushEvent() // 兜底：残留事件。转换器在 finish 事件内已自带 message_stop，无需额外收尾
}

// writeClaudeFakeStream fake 流式：非流式缓冲完整后，以 Anthropic SSE 事件分帧发给客户端。
// 帧序列与 real 流一致：message_start → content_block_start/delta/stop... → message_delta → message_stop。
func writeClaudeFakeStream(w http.ResponseWriter, out map[string]any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONResponse(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	emit := func(event string, data map[string]any) {
		_, _ = w.Write([]byte("event: " + event + "\n"))
		_, _ = w.Write([]byte("data: " + string(mustMarshal(data)) + "\n\n"))
		flusher.Flush()
	}

	msgID, _ := out["id"].(string)
	model, _ := out["model"].(string)
	stopReason, _ := out["stop_reason"].(string)
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := map[string]any{}
	if u, ok := out["usage"].(map[string]any); ok {
		usage = u
	}

	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"type":          "message",
			"id":            msgID,
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	})

	idx := 0
	blocks, _ := out["content"].([]any)
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "thinking":
			thinking, _ := block["thinking"].(string)
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		case "text":
			text, _ := block["text"].(string)
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input, _ := block["input"].(map[string]any)
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": input},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(mustMarshal(input))},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		}
	}

	outputTokens := float64(0)
	if v, ok := usage["output_tokens"].(float64); ok {
		outputTokens = v
	}
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}