package gemini

import (
	"encoding/json"
	"testing"
)

// ==================== Claude → Gemini（请求侧） ====================

func TestTranslateClaudeToGemini_Rich(t *testing.T) {
	req := mustReq(t, `{
		"model": "gemini-2.5-flash",
		"system": "You are helpful",
		"max_tokens": 2048,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "need weather"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}},
				{"type": "text", "text": "checking..."}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "20C"}
			]},
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"tools": [
			{"type": "custom", "name": "get_weather", "description": "get weather",
			 "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "web_search_20250305", "name": "web_search"},
			{"type": "web_fetch_20250305", "name": "web_fetch"},
			{"type": "code_execution_20250305", "name": "code_execution"}
		],
		"tool_choice": {"type": "tool", "name": "get_weather"},
		"output_format": {"type": "json_schema", "schema": {"type": "object", "properties": {"ok": {"type": "boolean"}}}}
	}`)

	g, suffix, err := TranslateClaudeToGemini(req, ConvertOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if suffix.CleanModel != "gemini-2.5-flash" {
		t.Fatalf("cleanModel = %s", suffix.CleanModel)
	}

	// system → systemInstruction
	if g.SystemInstruction == nil || len(g.SystemInstruction.Parts) != 1 ||
		g.SystemInstruction.Parts[0].Text == nil || *g.SystemInstruction.Parts[0].Text != "You are helpful" {
		t.Fatalf("systemInstruction 异常: %+v", g.SystemInstruction)
	}

	// generationConfig.maxOutputTokens
	if g.GenerationConfig == nil || g.GenerationConfig.MaxOutputTokens != 2048 {
		t.Fatalf("maxOutputTokens 异常: %+v", g.GenerationConfig)
	}
	// output_format → responseSchema + mime
	if g.GenerationConfig.ResponseMimeType != "application/json" || g.GenerationConfig.ResponseSchema == nil {
		t.Fatalf("responseSchema 异常: %+v", g.GenerationConfig)
	}

	// 3 条 contents：assistant(tool_use) / user(functionResponse) / user(hi)
	if len(g.Contents) != 3 {
		t.Fatalf("contents 数量 = %d, want 3: %+v", len(g.Contents), g.Contents)
	}
	asst := g.Contents[0]
	if asst.Role != "model" {
		t.Fatalf("assistant role = %q", asst.Role)
	}
	if len(asst.Parts) != 3 {
		t.Fatalf("assistant parts = %d: %+v", len(asst.Parts), asst.Parts)
	}
	// 顺序：thought → functionCall(带签名) → text
	if asst.Parts[0].Thought == nil || !*asst.Parts[0].Thought || asst.Parts[0].Text == nil || *asst.Parts[0].Text != "need weather" {
		t.Fatalf("thought part 异常: %+v", asst.Parts[0])
	}
	fc := asst.Parts[1].FunctionCall
	if fc == nil || fc.Name != "get_weather" || asst.Parts[1].ThoughtSignature != DummyThoughtSignature {
		t.Fatalf("functionCall(签名) 异常: %+v", asst.Parts[1])
	}
	// 后续 user functionResponse 不带签名（signature 只上首个 functionCall）

	// tool_result → functionResponse，按 tool_use_id 反查原名
	fr := g.Contents[1].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "get_weather" {
		t.Fatalf("functionResponse 异常: %+v", g.Contents[1].Parts[0])
	}
	if g.Contents[1].Parts[0].FunctionCall != nil {
		t.Fatalf("functionResponse 应不含 functionCall: %+v", g.Contents[1].Parts[0])
	}

	if len(g.Contents[2].Parts) != 1 || g.Contents[2].Parts[0].Text == nil || *g.Contents[2].Parts[0].Text != "hi" {
		t.Fatalf("末尾 text 异常: %+v", g.Contents[2].Parts)
	}

	// tools → 1 自定义 decl + 3 内置工具（web_search→GoogleSearch、web_fetch→URLContext、code_execution→CodeExecution）
	if len(g.Tools) != 4 {
		t.Fatalf("tools = %d, want 4: %+v", len(g.Tools), g.Tools)
	}
	var hasGoogle, hasURL, hasCode bool
	for _, tl := range g.Tools {
		if len(tl.FunctionDeclarations) == 1 && tl.FunctionDeclarations[0].Name == "get_weather" {
			continue
		}
		if tl.GoogleSearch != nil {
			hasGoogle = true
		}
		if tl.URLContext != nil {
			hasURL = true
		}
		if tl.CodeExecution != nil {
			hasCode = true
		}
	}
	if !hasGoogle || !hasURL || !hasCode {
		t.Fatalf("内置工具映射缺失: google=%v url=%v code=%v", hasGoogle, hasURL, hasCode)
	}
	// tool_choice → toolConfig.ANY + allowed
	if g.ToolConfig == nil || g.ToolConfig.FunctionCallingConfig == nil ||
		g.ToolConfig.FunctionCallingConfig.Mode != "ANY" ||
		len(g.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) != 1 ||
		g.ToolConfig.FunctionCallingConfig.AllowedFunctionNames[0] != "get_weather" {
		t.Fatalf("toolConfig 异常: %+v", g.ToolConfig)
	}
}

// web_search/web_fetch/code_execution 内置工具 → force 开关（不入 functionDeclarations）
func TestTranslateClaudeToGemini_ForceBuiltins(t *testing.T) {
	req := mustReq(t, `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "book a flight"}],
		"tools": [
			{"type": "web_search_20250305", "name": "web_search"},
			{"type": "web_fetch_20250305", "name": "web_fetch"},
			{"type": "code_execution_20250305", "name": "code_execution"}
		]
	}`)
	g, _, err := TranslateClaudeToGemini(req, ConvertOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// 内置工具（web_search/web_fetch/code_execution）→ 独立 tool 条目，不进 functionDeclarations
	var hasGoogle, hasURL, hasCode bool
	for _, tl := range g.Tools {
		if tl.GoogleSearch != nil {
			hasGoogle = true
		}
		if tl.URLContext != nil {
			hasURL = true
		}
		if tl.CodeExecution != nil {
			hasCode = true
		}
	}
	if !hasGoogle || !hasURL || !hasCode {
		t.Fatalf("内置工具映射缺失: google=%v url=%v code=%v", hasGoogle, hasURL, hasCode)
	}
	for _, tl := range g.Tools {
		if len(tl.FunctionDeclarations) != 0 {
			t.Fatalf("内置工具不应进 decls: %+v", g.Tools)
		}
	}
}

func TestTranslateClaudeToGemini_MaxTokensFallback(t *testing.T) {
	// 无 max_tokens：TranslateClaudeToGemini 保持 MaxOutputTokens=0，由 server 层用 config 兜底；
	// 转换本身不因缺失报错。
	req := mustReq(t, `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)
	g, _, err := TranslateClaudeToGemini(req, ConvertOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if g.GenerationConfig.MaxOutputTokens != 0 {
		t.Fatalf("未兜底时不应注入 maxOutputTokens: %+v", g.GenerationConfig)
	}
}

// ==================== Gemini → Claude（非流式响应） ====================

func TestTranslateGeminiToClaude_NonStream(t *testing.T) {
	sig := "sig-xyz"
	resp := GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Parts: []GeminiPart{
				thoughtPart("deep thinking"),
				textPart("Hello"),
				functionCallPart("get_weather", map[string]any{"city": "Paris"}, ""),
			}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &GeminiUsageMetadata{
			PromptTokenCount:     5,
			CandidatesTokenCount: 2,
			ThoughtsTokenCount:   3,
			TotalTokenCount:      15,
		},
	}
	// 手写 sig 校验：thought 默认补 DummyThoughtSignature，但这里显式走 thoughtPart(无 sig) → 转换应补默认
	// 先构造带 sign 的版本
	resp.Candidates[0].Content.Parts[0] = GeminiPart{Thought: boolPtr(true), Text: strPtr("deep thinking"), ThoughtSignature: sig}

	out := TranslateGeminiToClaude(&resp, "gemini-2.5-flash")
	b, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Fatalf("message = %v", m)
	}
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", m["stop_reason"])
	}
	content := m["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content blocks = %d: %v", len(content), content)
	}
	th := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "deep thinking" || th["signature"] != sig {
		t.Fatalf("thinking block = %v", th)
	}
	tx := content[1].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "Hello" {
		t.Fatalf("text block = %v", tx)
	}
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "get_weather" {
		t.Fatalf("tool_use block = %v", tu)
	}
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestTranslateGeminiToClaude_MaxTokensStopReason(t *testing.T) {
	resp := GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content:      &GeminiContent{Parts: []GeminiPart{textPart("partial")}},
			FinishReason: "MAX_TOKENS",
		}},
	}
	out := TranslateGeminiToClaude(&resp, "g")
	if out["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason = %v, want max_tokens", out["stop_reason"])
	}
}

// ==================== Gemini → Claude（流式） ====================

type claudeEvent struct {
	event string
	data  map[string]any
}

// parseClaudeEvents 解析 Anthropic event:/data: SSE 帧
func parseClaudeEvents(t *testing.T, sse []byte) []claudeEvent {
	t.Helper()
	var events []claudeEvent
	for _, blk := range splitSSE(sse) {
		if blk == "" {
			continue
		}
		var ev, data string
		for _, line := range splitLines(blk) {
			switch {
			case len(line) >= 7 && line[:7] == "event: ":
				ev = line[7:]
			case len(line) >= 6 && line[:6] == "data: ":
				data = line[6:]
			}
		}
		if data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("解析 data 帧失败 %q: %v", data, err)
		}
		events = append(events, claudeEvent{event: ev, data: m})
	}
	return events
}

func splitSSE(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			// 空行 = 事件结束
			if i-1 >= 0 && b[i-1] == '\n' {
				out = append(out, string(b[start:i]))
				start = i + 1
				for start < len(b) && b[start] == '\n' {
					start++
				}
			}
		}
	}
	if start < len(b) && string(b[start:len(b)]) != "" {
		out = append(out, string(b[start:len(b)]))
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(ch)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

var claudeStreamChunks = []string{
	`{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking...","thoughtSignature":"sig-a"}]}}]}`,
	`{"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}`,
	`{"candidates":[{"content":{"parts":[{"text":"world"}]}}]}`,
	`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":10}}`,
}

func TestTranslateGeminiToClaudeStream(t *testing.T) {
	st := &ClaudeStreamState{}
	var all []byte
	for _, raw := range claudeStreamChunks {
		out, err := TranslateGeminiToClaudeStream([]byte(raw), "gemini-2.5-flash", st)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, out...)
	}
	events := parseClaudeEvents(t, all)

	var types []string
	for _, e := range events {
		types = append(types, e.event)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking（独立块，文本前关闭）
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // text（2 段增量）
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

	// message_start：一次性、模型正确
	ms := events[0].data
	if ms["type"] != "message_start" {
		t.Fatalf("ms = %v", ms)
	}
	msg := ms["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["model"] != "gemini-2.5-flash" {
		t.Fatalf("message_start = %v", msg)
	}

	// thinking delta
	td := events[2].data["delta"].(map[string]any)
	if td["type"] != "thinking_delta" || td["thinking"] != "thinking..." {
		t.Fatalf("thinking delta = %v", events[2].data)
	}
	if events[1].data["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatalf("thinking start = %v", events[1].data)
	}

	// 两个 text delta 合并进同一 text block，stop 在 finish 帧内
	tx1 := events[5].data["delta"].(map[string]any)
	tx2 := events[6].data["delta"].(map[string]any)
	if tx1["type"] != "text_delta" || tx1["text"] != "Hello " || tx2["text"] != "world" {
		t.Fatalf("text deltas = %v / %v", tx1, tx2)
	}

	// finish：message_delta stop_reason end_turn + usage
	md := events[8].data
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta = %v", md)
	}
	if md["usage"].(map[string]any)["output_tokens"] != float64(5) {
		t.Fatalf("usage = %v", md["usage"])
	}
	if events[9].data["type"] != "message_stop" {
		t.Fatalf("末帧 = %v", events[9].data)
	}
}

// 工具调用流：tool_use block → content_block_start/input_json_delta/stop，stop_reason=tool_use
func TestTranslateGeminiToClaudeStream_ToolCall(t *testing.T) {
	st := &ClaudeStreamState{}
	var all []byte
	for _, raw := range []string{
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`,
	} {
		out, _ := TranslateGeminiToClaudeStream([]byte(raw), "g", st)
		all = append(all, out...)
	}
	events := parseClaudeEvents(t, all)

	var onStart bool
	var sawDelta, sawStop bool
	stopReason := ""
	for _, e := range events {
		switch e.event {
		case "content_block_start":
			if cb := e.data["content_block"].(map[string]any); cb["type"] == "tool_use" {
				onStart = true
				if cb["name"] != "get_weather" || cb["id"] == "" {
					t.Fatalf("tool_use start = %v", cb)
				}
			}
		case "content_block_delta":
			if d := e.data["delta"].(map[string]any); d["type"] == "input_json_delta" {
				sawDelta = true
			}
		case "content_block_stop":
			sawStop = true
		case "message_delta":
			stopReason, _ = e.data["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	if !onStart || !sawDelta || !sawStop {
		t.Fatalf("缺 tool_use 帧：start=%v delta=%v stop=%v\n%v", onStart, sawDelta, sawStop, events)
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stopReason)
	}
	if !st.HasToolUse {
		t.Fatalf("HasToolUse 应置位")
	}
}

// 空 content 保护：无候选/无 parts 不产生错误帧
func TestTranslateGeminiToClaudeStream_Empty(t *testing.T) {
	st := &ClaudeStreamState{}
	out, err := TranslateGeminiToClaudeStream([]byte(`{"candidates":[]}`), "g", st)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("空候选应无输出: %s", out)
	}
}

func strPtr(s string) *string { return &s }