package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ==================== Anthropic /v1/messages → Gemini ====================

// Anthropic 非流式：Claude 协议请求 → Gemini → Anthropic message 响应
func TestE2E_ClaudeNonStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-claude-ns-key-0001"}))

	w := doPost(t, rt, "/v1/messages", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Fatalf("message = %v", resp)
	}
	content := resp["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "Hello from Gemini" {
		t.Fatalf("content = %v", content)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", resp["stop_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(9) {
		t.Fatalf("usage = %v", usage)
	}

	// 上游路径与 Gemini 请求体（contents 结构 + maxOutputTokens 由 Claude max_tokens 映射）
	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	if posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("upstream path = %q", posts[0].path)
	}
	var gr map[string]any
	if err := json.Unmarshal(posts[0].body, &gr); err != nil {
		t.Fatal(err)
	}
	genCfg := gr["generationConfig"].(map[string]any)
	if genCfg["maxOutputTokens"] != float64(100) {
		t.Fatalf("maxOutputTokens = %v", genCfg["maxOutputTokens"])
	}
	if _, ok := gr["contents"]; !ok {
		t.Fatalf("upstream body 应为 Gemini contents: %s", posts[0].body)
	}
}

// Anthropic 流式：SSE 帧序列 message_start → content_block_* → message_delta → message_stop
func TestE2E_ClaudeStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-claude-str-key-0001"}))

	w := doPost(t, rt, "/v1/messages", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	events := parseAnthropicSSE(t, w.Body.Bytes())

	var types []string
	for _, e := range events {
		types = append(types, e.event)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // text
		"message_delta", "message_stop",
	}
	if len(types) != len(want) {
		t.Fatalf("事件序列 = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("事件[%d] = %s, want %s（全序列 %v）", i, types[i], want[i], types)
		}
	}

	// message_start 携带 model 与 usage
	ms := events[0].data
	msg := ms["message"].(map[string]any)
	if msg["model"] != "gemini-2.5-flash" || msg["role"] != "assistant" {
		t.Fatalf("message_start = %v", msg)
	}
	// text delta 内容正确
	tx1 := events[5].data["delta"].(map[string]any)
	tx2 := events[6].data["delta"].(map[string]any)
	if tx1["text"] != "Hello " || tx2["text"] != "world" {
		t.Fatalf("text deltas = %v / %v", tx1, tx2)
	}
	// message_delta stop_reason + output_tokens（thoughts 并入）
	md := events[8].data
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta = %v", md)
	}
	if md["usage"].(map[string]any)["output_tokens"] != float64(8) {
		t.Fatalf("usage = %v", md["usage"])
	}
	// 无 OpenAI 的 [DONE] 标记
	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("Anthropic 流不应有 [DONE]")
	}
}

// Anthropic fake 流式：非流式缓冲后按 Anthropic SSE 分帧
func TestE2E_ClaudeFakeStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	cfg := geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-claude-fk-key-0001"})
	cfg.Gemini.StreamingMode = "fake"
	rt := newTestRouter(cfg)

	w := doPost(t, rt, "/v1/messages", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	events := parseAnthropicSSE(t, w.Body.Bytes())
	var types []string
	for _, e := range events {
		types = append(types, e.event)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(types) != len(want) {
		t.Fatalf("事件序列 = %v, want %v", types, want)
	}
	tx := events[2].data["delta"].(map[string]any)
	if tx["type"] != "text_delta" || tx["text"] != "Hello from Gemini" {
		t.Fatalf("text delta = %v", events[2].data)
	}
	// fake 模式走非流式端点
	if posts := fake.postCalls(); len(posts) != 1 || posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("fake 模式请求异常: %+v", posts)
	}
}

// Anthropic 确定性错误：透传，不烧 key
func TestE2E_ClaudeDeterministicError(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{400, geminiErrBody("INVALID_ARGUMENT", "Bad request body")})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-claude-det-key-0001"}))

	w := doPost(t, rt, "/v1/messages", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("应透传 Gemini 错误体: %s", w.Body.String())
	}
	if got := len(fake.postCalls()); got != 1 {
		t.Fatalf("确定性错误不应重试换 key, 生成调用次数 = %d", got)
	}
}

// claudeEventS parsed Anthropic SSE 事件（event:/data: 一对）
type claudeEventS struct {
	event string
	data  map[string]any
}

func parseAnthropicSSE(t *testing.T, body []byte) []claudeEventS {
	t.Helper()
	var events []claudeEventS
	for _, blk := range splitSSEBlk(body) {
		if blk == "" {
			continue
		}
		var ev, data string
		for _, line := range strings.Split(blk, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("解析 data 帧失败 %q: %v", data, err)
		}
		events = append(events, claudeEventS{event: ev, data: m})
	}
	return events
}

func splitSSEBlk(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' && i+1 < len(b) && b[i+1] == '\n' {
			out = append(out, string(b[start:i+1]))
			start = i + 2
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
