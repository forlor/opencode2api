package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"opencode2api/config"
	"opencode2api/gemini"
)

// geminiModelPattern 模型路径白名单：字母数字 _ . - : 与斜杠(容错)，拒绝空格/路径穿越
var geminiModelPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-:]+$`)

// streamIdleTimeout 上游 SSE 流无数据超时（与 OpenCode 线路一致）：超时主动断开
const streamIdleTimeout = 180 * time.Second

// sanitizeGeminiModel 净化模型名并去除 "models/" 前缀，非法名返回空串
func sanitizeGeminiModel(model string) string {
	m := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if !geminiModelPattern.MatchString(m) {
		return ""
	}
	return m
}

// writeProxyError 统一代理层错误响应（OpenAI error 格式，客户端 SDK 可解析）
func writeProxyError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": errType, "message": message, "code": status},
	})
}

// handleGeminiChat OpenAI /v1/chat/completions → Gemini 线路。
// 重试编排：外层轮询节点（最多 len(nodes) 次），内层在节点内换 key（最多该节点 key 数）；
// 错误分类——429 冷却换 key、403 无效 key Ban、5xx/连接失败退避换 key、
// 400/404 确定性业务错误直接透传不烧 key。
func (r *Router) handleGeminiChat(w http.ResponseWriter, req *http.Request, payload map[string]any, decision config.RouteDecision, bodyLen int) {
	gcfg := r.cfg.Gemini
	isStream, _ := payload["stream"].(bool)

	// OpenAI chat 常省略 max_tokens，而 Gemini 缺 maxOutputTokens 会 400 → 兜底
	if payload["max_tokens"] == nil {
		payload["max_tokens"] = gcfg.DefaultMaxOutputTokens
	}
	// 用分流决策的目标模型做转换（后缀剥离/上游 URL 都以它为准）
	payload["model"] = decision.TargetModel

	opts := gemini.ConvertOpts{
		ForceThinking:     gcfg.ForceThinking,
		ForceWebSearch:    gcfg.ForceWebSearch,
		ForceCodeExecution: gcfg.ForceCodeExecution,
		ForceUrlContext:   gcfg.ForceUrlContext,
		SafetyThreshold:   gcfg.SafetySettingsThreshold,
	}
	g, suffix, err := gemini.TranslateOpenAIToGemini(payload, opts)
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
			forwardGeminiStream(w, req, resp, decision.TargetModel)
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
		out := gemini.TranslateGeminiToOpenAI(&gr, decision.TargetModel)
		if isStream {
			writeOpenAIFakeStream(w, out)
		} else {
			writeJSONResponse(w, out)
		}
	})
}

// geminiSuccess 健康 200 时的成功处理（负责读体/转发并归还 key，返回后重试编排结束）
type geminiSuccess func(w http.ResponseWriter, req *http.Request, node *gemini.GeminiNode, key *gemini.GeminiKey, resp *http.Response)

// geminiExec 节点/key 轮询 + 错误分类 + 重试编排（OpenAI/Claude 协议共用）。
// 429 冷却换 key、403 无效 key Ban、5xx/连接失败退避换 key、
// 400/404 确定性业务错误直接透传不烧 key；健康 200 时调用 onSuccess 一次。
func (r *Router) geminiExec(w http.ResponseWriter, req *http.Request, gBody []byte, modelPath, endpoint string, onSuccess geminiSuccess) {
	nodes := r.gemPool.Nodes()
	if len(nodes) == 0 {
		writeProxyError(w, http.StatusServiceUnavailable, "proxy_error", "Gemini 线路未配置节点")
		return
	}

	backoff := time.Second // 临时错误指数退避：1s/2s/4s
	// countTokens 不消耗每日生成配额（Google RPD 只计生成请求），也不受耗尽限制
	chargeDaily := !strings.HasPrefix(endpoint, ":countTokens")
	for nodeTry := 0; nodeTry < len(nodes); nodeTry++ {
		node, err := r.gemPool.GetNextNode()
		if err != nil {
			if !sleepOrAbort(req.Context(), 100*time.Millisecond) {
				return
			}
			continue
		}
		for kt := 0; kt < node.KeyCount(); kt++ {
			key, err := node.GetNextKey(modelPath, chargeDaily)
			if err != nil {
				break // 节点内无可用 key → 换节点
			}

			httpReq, err := buildGeminiHTTPRequest(req.Context(), node, key, modelPath, endpoint, gBody)
			if err != nil {
				node.ReportConnectionFailure(key)
				node.ReleaseKey(key)
				break
			}
			resp, err := node.Client().Do(httpReq)
			if err != nil {
				node.ReportConnectionFailure(key)
				node.ReleaseKey(key)
				log.Printf("[gemini][%s] key %s 请求失败: %v", node.Name, key.ID(), err)
				if !sleepOrAbort(req.Context(), backoff) {
					return
				}
				if backoff < 4*time.Second {
					backoff *= 2
				}
				continue
			}

			// 流首错误预检（非 200 读全 + 200 SSE 扫前数行）
			errBody, kind, rerr := gemini.ReadAndCheckGeminiStreamError(resp)
			if rerr != nil {
				node.ReportConnectionFailure(key)
				node.ReleaseKey(key)
				resp.Body.Close()
				if !sleepOrAbort(req.Context(), backoff) {
					return
				}
				continue
			}

			switch kind {
			case gemini.ErrRateLimit:
				// 分钟级限流（RPM/TPM/未知型）：优先 Google RetryInfo.retryDelay 精确冷却，回退 key_cooldown
				backoff, _ := gemini.ParseRetryDelay(errBody)
				node.Report429(key, backoff)
				node.ReleaseKey(key)
				resp.Body.Close()
				log.Printf("[gemini][%s] key %s 429 限流，换 key", node.Name, key.ID())
				if !sleepOrAbort(req.Context(), 150*time.Millisecond) {
					return
				}
				continue
			case gemini.ErrDailyQuota:
				// 每日配额（RPD）型 429：该 key 该模型标记耗尽到次日重置，直接换 key。
				// 不退避 sleep——一次请求可能连续耗尽多个 key，避免叠加延迟
				node.Report429Daily(key, modelPath)
				node.ReleaseKey(key)
				resp.Body.Close()
				log.Printf("[gemini][%s] key %s 模型 %s 当日配额耗尽，已标记至次日重置，换 key", node.Name, key.ID(), modelPath)
				continue
			case gemini.ErrInvalidKey:
				node.ReportInvalidKey(key, "403 API key not valid")
				node.ReleaseKey(key)
				resp.Body.Close()
				log.Printf("[gemini][%s] key %s 无效，已 Ban", node.Name, key.ID())
				continue
			case gemini.ErrTemporary:
				node.Report5xx(key)
				node.ReleaseKey(key)
				resp.Body.Close()
				log.Printf("[gemini][%s] key %s 上游临时错误，退避后重试", node.Name, key.ID())
				if !sleepOrAbort(req.Context(), backoff) {
					return
				}
				if backoff < 4*time.Second {
					backoff *= 2
				}
				continue
			case gemini.ErrDeterministic:
				// 确定性业务错误（400/404/422/安全拦截）：透传，不烧 key
				node.ReleaseKey(key)
				status := resp.StatusCode
				if status == http.StatusOK {
					status = geminiErrorCode(errBody)
				}
				if status <= 0 {
					status = http.StatusBadRequest
				}
				log.Printf("[gemini][%s] 确定性错误 %d 透传: %s", node.Name, status, truncateLog(errBody))
				writeRawJSON(w, status, errBody)
				resp.Body.Close()
				return
			}

			// 健康 200
			node.Report200(key)
			onSuccess(w, req, node, key, resp)
			return
		}
	}

	writeProxyError(w, http.StatusBadGateway, "proxy_error", "所有 Gemini 节点/key 均不可用（限流、当日配额耗尽或连接失败）")
}

// buildGeminiHTTPRequest 构造发往节点 nginx 的 Gemini REST 请求（key 走 Header 不落 access_log）。
// 鉴权与 upstream_host 路由头统一由 node.ApplyOutboundHeaders 收口。
func buildGeminiHTTPRequest(ctx context.Context, node *gemini.GeminiNode, key *gemini.GeminiKey, modelPath, endpoint string, body []byte) (*http.Request, error) {
	u := node.BaseURL() + "/v1beta/models/" + modelPath + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-goog-api-key", key.Value())
	node.ApplyOutboundHeaders(req)
	return req, nil
}

// geminiErrorCode 从 Gemini 错误体提取 HTTP 码（200-SSE 包错误时用）
func geminiErrorCode(body []byte) int {
	if gerr, ok := gemini.ParseGeminiError(body); ok && gerr.Code != 0 {
		return gerr.Code
	}
	return 0
}

// sleepOrAbort 退避等待；客户端断开时返回 false
func sleepOrAbort(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// forwardGeminiStream 把 Gemini SSE 流实时转换为 OpenAI chat.completion.chunk SSE 流。
// 只在本函数被调用前已确认上游健康；中途上游 error → 发一条代理错误帧后结束，不重放。
func forwardGeminiStream(w http.ResponseWriter, req *http.Request, resp *http.Response, model string) {
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
	st := &gemini.OpenAIStreamState{}
	var pending []byte
	hasPending := false
	streamAborted := false

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
		// 上游中途 error 事件 → 发代理错误帧并终止
		if gerr, ok := gemini.ParseGeminiError(raw); ok {
			streamAborted = true
			log.Printf("[gemini] 流中途上游错误: %s %s", gerr.Status, truncateLog([]byte(gerr.Message)))
			msg := fmt.Sprintf("[ProxySystem Error] %s %s", gerr.Status, gerr.Message)
			_, _ = w.Write([]byte("data: " + string(mustMarshal(map[string]any{"error": map[string]any{"message": msg}})) + "\n\n"))
			flusher.Flush()
			return
		}
		out, _ := gemini.TranslateGeminiToOpenAIStream(raw, model, st)
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
			// 其他行（注释/event:）忽略
		}
		if err != nil {
			break
		}
	}
	flushEvent() // 兜底：残留事件
	if !streamAborted {
		// OpenAI SSE 标准结束帧
		if _, werr := w.Write([]byte("data: [DONE]\n\n")); werr == nil {
			flusher.Flush()
		}
	}
}

// readLineRemaining 配合 ReadSlice 的 ErrBufferFull 读完整行
func readLineRemaining(reader *bufio.Reader) []byte {
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

// writeOpenAIFakeStream fake 流式：非流式请求缓冲完整后，以 OpenAI SSE 分帧发给客户端
func writeOpenAIFakeStream(w http.ResponseWriter, out map[string]any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONResponse(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	id, _ := out["id"].(string)
	created, _ := out["created"].(int64)
	model, _ := out["model"].(string)
	usage := out["usage"]

	var msg map[string]any
	finish := "stop"
	if choices, ok := out["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if m, ok := c0["message"].(map[string]any); ok {
				msg = m
			}
			if fr, ok := c0["finish_reason"].(string); ok {
				finish = fr
			}
		}
	}

	chunk := func(delta map[string]any, finish any) {
		frame := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if finish != nil {
			frame["usage"] = usage
		}
		_, _ = w.Write([]byte("data: " + string(mustMarshal(frame)) + "\n\n"))
		flusher.Flush()
	}

	delta := map[string]any{"role": "assistant"}
	if rc, ok := msg["reasoning_content"]; ok {
		delta["reasoning_content"] = rc
	}
	if tc, ok := msg["tool_calls"]; ok {
		delta["tool_calls"] = tc
	}
	if c, ok := msg["content"]; ok {
		delta["content"] = c
	}
	chunk(delta, nil)
	chunk(map[string]any{}, finish)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// writeJSONResponse 直接输出非流式 OpenAI 响应
func writeJSONResponse(w http.ResponseWriter, out map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// writeRawJSON 原样透传上游错误体（带状态码）
func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	if len(body) == 0 {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}