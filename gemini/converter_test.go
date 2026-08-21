package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustReq(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test json: %v", err)
	}
	return m
}

func TestParseModelSuffixes(t *testing.T) {
	cases := []struct {
		model        string
		wantClean    string
		wantLevel    string
		wantStream   string
		wantSearch   bool
		wantCode     bool
	}{
		{"gemini-3-flash-preview-minimal-real-search", "gemini-3-flash-preview", "MINIMAL", "real", true, false},
		{"gemini-2.5-flash", "gemini-2.5-flash", "", "", false, false},
		{"gemini-3-pro-preview(high)-fake-code", "gemini-3-pro-preview", "HIGH", "fake", false, true},
		{"gemini-x-search-code", "gemini-x", "", "", true, true},
		{"gemini-x-real", "gemini-x", "", "real", false, false},
		{"", "", "", "", false, false},
	}
	for _, c := range cases {
		info := ParseModelSuffixes(c.model)
		if info.CleanModel != c.wantClean {
			t.Errorf("ParseModelSuffixes(%q).CleanModel = %q, want %q", c.model, info.CleanModel, c.wantClean)
		}
		if info.ThinkingLevel != c.wantLevel {
			t.Errorf("ParseModelSuffixes(%q).ThinkingLevel = %q, want %q", c.model, info.ThinkingLevel, c.wantLevel)
		}
		if info.StreamingMode != c.wantStream {
			t.Errorf("ParseModelSuffixes(%q).StreamingMode = %q, want %q", c.model, info.StreamingMode, c.wantStream)
		}
		if info.ForceWebSearch != c.wantSearch {
			t.Errorf("ParseModelSuffixes(%q).ForceWebSearch = %v, want %v", c.model, info.ForceWebSearch, c.wantSearch)
		}
		if info.ForceCodeExecution != c.wantCode {
			t.Errorf("ParseModelSuffixes(%q).ForceCodeExecution = %v, want %v", c.model, info.ForceCodeExecution, c.wantCode)
		}
	}
}

func TestTranslateOpenAIToGemini_Rich(t *testing.T) {
	req := mustReq(t, `{
		"model": "gemini-3-flash-preview-minimal-real-search",
		"messages": [
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hi"},
			{"role":"assistant","content":"Hello","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"{\"temp\": 20}"},
			{"role":"tool","tool_call_id":"call_2","name":"get_time","content":"12:00"},
			{"role":"user","content":"thanks"}
		],
		"tools": [
			{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},
			{"type":"function","function":{"name":"get_time","description":"Get time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}
		],
		"max_tokens": 100,
		"temperature": 0.7
	}`)

	g, suffix, err := TranslateOpenAIToGemini(req, ConvertOpts{SafetyThreshold: "OFF"})
	if err != nil {
		t.Fatal(err)
	}
	if suffix.CleanModel != "gemini-3-flash-preview" {
		t.Fatalf("cleanModel = %q", suffix.CleanModel)
	}
	if suffix.ThinkingLevel != "MINIMAL" || suffix.StreamingMode != "real" || !suffix.ForceWebSearch {
		t.Fatalf("suffix 解析异常: %+v", suffix)
	}

	// system
	if g.SystemInstruction == nil || len(g.SystemInstruction.Parts) != 1 ||
		*g.SystemInstruction.Parts[0].Text != "You are helpful." {
		t.Fatalf("systemInstruction 异常: %+v", g.SystemInstruction)
	}

	// contents 结构：user Hi / model [2 funcCall, text "Hello"] / user [2 functionResponse] / user thanks
	if len(g.Contents) != 4 {
		t.Fatalf("contents 数量 = %d, want 4", len(g.Contents))
	}
	if g.Contents[1].Role != "model" {
		t.Fatalf("contents[1].Role = %q", g.Contents[1].Role)
	}
	calls := g.Contents[1].Parts
	if len(calls) != 3 {
		t.Fatalf("model parts = %d, want 3 (2 funcCall + text)", len(calls))
	}
	if calls[0].FunctionCall == nil || calls[0].FunctionCall.Name != "get_weather" {
		t.Fatalf("funcCall[0] 异常: %+v", calls[0])
	}
	if calls[0].ThoughtSignature != DummyThoughtSignature {
		t.Fatalf("首个 functionCall 应带 thoughtSignature，得到 %q", calls[0].ThoughtSignature)
	}
	if calls[1].FunctionCall == nil || calls[1].FunctionCall.Name != "get_time" {
		t.Fatalf("funcCall[1] 异常: %+v", calls[1])
	}
	if calls[1].ThoughtSignature != "" {
		t.Fatalf("第二个 functionCall 不应带 thoughtSignature，得到 %q", calls[1].ThoughtSignature)
	}
	if calls[2].Text == nil || *calls[2].Text != "Hello" {
		t.Fatalf("assistant 正文应保留: %+v", calls[2])
	}
	// tool 响应合并为单 user
	resps := g.Contents[2]
	if resps.Role != "user" || len(resps.Parts) != 2 {
		t.Fatalf("tool 响应应合并为单 user 2 个 part，得到 role=%s parts=%d", resps.Role, len(resps.Parts))
	}
	if resps.Parts[0].FunctionResponse == nil || resps.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Fatalf("functionResponse[0] 异常: %+v", resps.Parts[0])
	}

	// generationConfig
	gc := g.GenerationConfig
	if gc == nil || gc.MaxOutputTokens != 100 {
		t.Fatalf("maxOutputTokens = %v", gc)
	}
	if gc.Temperature == nil || *gc.Temperature != 0.7 {
		t.Fatalf("temperature 异常: %+v", gc.Temperature)
	}
	if gc.ThinkingConfig == nil || gc.ThinkingConfig.ThinkingLevel != "MINIMAL" {
		t.Fatalf("thinkingConfig 异常: %+v", gc.ThinkingConfig)
	}

	// tools：functionDeclarations + force googleSearch
	if !hasFunctionDeclarations(g.Tools) {
		t.Fatalf("应有 functionDeclarations: %+v", g.Tools)
	}
	if !hasGeminiTool(g.Tools, "googleSearch") {
		t.Fatalf("应有 googleSearch: %+v", g.Tools)
	}
	// 同时存在 → includeServerSideToolInvocations
	if g.ToolConfig == nil || g.ToolConfig.IncludeServerSideToolInvocations == nil ||
		!*g.ToolConfig.IncludeServerSideToolInvocations {
		t.Fatalf("应启用 includeServerSideToolInvocations: %+v", g.ToolConfig)
	}
	// safety
	if len(g.SafetySettings) != 4 {
		t.Fatalf("safetySettings 应为 4 条, got %d", len(g.SafetySettings))
	}
}

func TestTranslateOpenAIToGemini_ImageDataURL(t *testing.T) {
	req := mustReq(t, `{
		"model":"gemini-2.5-flash",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}]
	}`)
	g, _, err := TranslateOpenAIToGemini(req, ConvertOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Contents) != 1 {
		t.Fatalf("contents = %d", len(g.Contents))
	}
	parts := g.Contents[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil {
		t.Fatalf("应包含 text + inlineData: %+v", parts)
	}
	if parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "AAAA" {
		t.Fatalf("inlineData 异常: %+v", parts[1].InlineData)
	}
}

func TestTranslateOpenAIToGemini_ResponseFormat(t *testing.T) {
	req := mustReq(t, `{
		"model":"gemini-2.5-flash",
		"messages":[{"role":"user","content":"x"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{
			"type":"object",
			"properties":{"name":{"type":"string","nullable":true}},
			"required":["name"]
		}}}
	}`)
	g, _, err := TranslateOpenAIToGemini(req, ConvertOpts{})
	if err != nil {
		t.Fatal(err)
	}
	gc := g.GenerationConfig
	if gc.ResponseMimeType != "application/json" {
		t.Fatalf("responseMimeType = %q", gc.ResponseMimeType)
	}
	schema, ok := gc.ResponseSchema.(map[string]any)
	if !ok {
		t.Fatalf("responseSchema 非 map: %T", gc.ResponseSchema)
	}
	if schema["type"] != "OBJECT" {
		t.Fatalf("schema.type = %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties 缺失")
	}
	name, ok := props["name"].(map[string]any)
	if !ok {
		t.Fatalf("property name 缺失")
	}
	if name["type"] != "STRING" || name["nullable"] != true {
		t.Fatalf("property name 转换异常: %+v", name)
	}
}

func TestTranslateGeminiToOpenAI_NonStream(t *testing.T) {
	resp := GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Parts: []GeminiPart{
				textPart("Hello"),
			}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &GeminiUsageMetadata{
			PromptTokenCount:     5,
			CandidatesTokenCount: 7,
			ThoughtsTokenCount:   3,
			TotalTokenCount:      15,
		},
	}
	out := TranslateGeminiToOpenAI(&resp, "gemini-2.5-flash")
	b, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "chat.completion" {
		t.Fatalf("object = %v", m["object"])
	}
	choices := m["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello" || msg["role"] != "assistant" {
		t.Fatalf("message = %v", msg)
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(10) {
		t.Fatalf("usage = %v", usage)
	}
	cd := usage["completion_tokens_details"].(map[string]any)
	if cd["reasoning_tokens"] != float64(3) {
		t.Fatalf("reasoning_tokens = %v", cd["reasoning_tokens"])
	}
}

func TestTranslateGeminiToOpenAI_NonStream_ToolCall(t *testing.T) {
	resp := GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Parts: []GeminiPart{
				functionCallPart("get_weather", map[string]any{"city": "Paris"}, ""),
			}},
			FinishReason: "STOP",
		}},
	}
	out := TranslateGeminiToOpenAI(&resp, "g")
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if m["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls")
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d", len(tcs))
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("tool_call 异常: %v", fn)
	}
}

// parseFrames 把 SSE 字节流按 \n\n 拆成一帧帧 data JSON map
func parseFrames(t *testing.T, sse []byte) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, raw := range strings.Split(string(sse), "\n\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if line == "data: [DONE]" {
			frames = append(frames, map[string]any{"__done": true})
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatalf("解析帧失败 %q: %v", line, err)
		}
		frames = append(frames, m)
	}
	return frames
}

func TestTranslateGeminiToOpenAIStream(t *testing.T) {
	st := &OpenAIStreamState{}
	var buf []byte

	// 帧1：thinking
	out1, err := TranslateGeminiToOpenAIStream([]byte(
		`{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking..."}]}}]}`), "m", st)
	if err != nil {
		t.Fatal(err)
	}
	buf = append(buf, out1...)

	// 帧2：正文（无 role）
	out2, _ := TranslateGeminiToOpenAIStream([]byte(
		`{"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}`), "m", st)
	buf = append(buf, out2...)

	// 帧3：functionCall
	out3, _ := TranslateGeminiToOpenAIStream([]byte(
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"a":1}}}]}}]}`), "m", st)
	buf = append(buf, out3...)

	// 帧4：finish + usage
	out4, _ := TranslateGeminiToOpenAIStream([]byte(
		`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2,"totalTokenCount":13}}`), "m", st)
	buf = append(buf, out4...)

	frames := parseFrames(t, buf)
	if len(frames) != 4 { // thinking, text, toolcall, finish
		t.Fatalf("帧数 = %d, want 4", len(frames))
	}

	// 帧1 应带 role
	f0 := frames[0]
	delta0 := f0["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta0["role"] != "assistant" || delta0["reasoning_content"] != "thinking..." {
		t.Fatalf("帧1 delta 异常: %v", delta0)
	}
	f1 := frames[1]
	delta1 := f1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, hasRole := delta1["role"]; hasRole {
		t.Fatalf("帧2 不应重复 role: %v", delta1)
	}
	if delta1["content"] != "Hello " {
		t.Fatalf("帧2 content = %v", delta1["content"])
	}
	// 帧3 tool_calls index 0
	f2 := frames[2]
	delta2 := f2["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	tcs := delta2["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["index"] != float64(0) || tc["type"] != "function" {
		t.Fatalf("帧3 tool_call 异常: %v", tc)
	}
	// 帧4 finish_reason（流内有 functionCall → tool_calls）+ usage
	f3 := frames[3]
	ch3 := f3["choices"].([]any)[0].(map[string]any)
	if ch3["finish_reason"] != "tool_calls" {
		t.Fatalf("帧4 finish_reason = %v, want tool_calls（流中有 functionCall）", ch3["finish_reason"])
	}
	usage := f3["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(8) {
		t.Fatalf("帧4 usage 异常: %v", usage)
	}
	// id 一致
	if f0["id"] != frames[3]["id"] {
		t.Fatalf("流 id 应一致")
	}
}

func TestTranslateGeminiToOpenAIStream_ToolCallFinishReason(t *testing.T) {
	st := &OpenAIStreamState{}
	var buf []byte
	b1, _ := TranslateGeminiToOpenAIStream([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}]}`), "m", st)
	buf = append(buf, b1...)
	b2, _ := TranslateGeminiToOpenAIStream([]byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`), "m", st)
	buf = append(buf, b2...)
	frames := parseFrames(t, buf)
	last := frames[len(frames)-1]
	if last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("有 functionCall 时 finish_reason 应为 tool_calls，得到 %v", last["choices"])
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"MAX_TOKENS": "length", "SAFETY": "content_filter", "STOP": "stop", "RECITATION": "stop", "OTHER": "stop", "": "stop",
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertSchemaToGemini_NullableAnyOf(t *testing.T) {
	// 工具参数 schema: nullable + anyOf
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": []any{"string", "null"}},
			"b": map[string]any{"anyOf": []any{
				map[string]any{"type": "integer"},
				map[string]any{"type": "null"},
			}},
		},
	}
	out := ConvertSchemaToGemini(in, false, false)
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["type"] != "OBJECT" {
		t.Fatalf("type = %v", m["type"])
	}
	props := m["properties"].(map[string]any)
	pa := props["a"].(map[string]any)
	if pa["type"] != "STRING" || pa["nullable"] != true {
		t.Fatalf("nullable type 数组处理异常: %v", pa)
	}
	pb := props["b"].(map[string]any)
	if pb["nullable"] != true || pb["type"] != "INTEGER" {
		t.Fatalf("anyOf 折叠异常: %v", pb)
	}
}

func TestConvertSchemaToGemini_ResponseFilters(t *testing.T) {
	in := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
		"properties": map[string]any{
			"x": map[string]any{
				"type":    []any{"string", "null"},
				"default": "d", // isResponseSchema 时应被过滤
				"enum":    []any{"a", "b"},
			},
		},
	}
	out := ConvertSchemaToGemini(in, true, false)
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, has := m["$schema"]; has {
		t.Fatalf("$schema 应被过滤: %v", m)
	}
	props := m["properties"].(map[string]any)
	x := props["x"].(map[string]any)
	if _, has := x["default"]; has {
		t.Fatalf("default 应被过滤: %v", x)
	}
	// enum 字符串化 + type=STRING
	if x["type"] != "STRING" {
		t.Fatalf("enum 时 type 应为 STRING: %v", x)
	}
	enum := x["enum"].([]any)
	for i, v := range enum {
		if v != "a" && v != "b" {
			t.Fatalf("enum 应字符串化, [%d]=%v", i, v)
		}
	}
}

func TestNormalizeToolResponse(t *testing.T) {
	if r := normalizeToolResponse(`{"temp":20}`); r.(map[string]any)["temp"] != float64(20) {
		t.Fatalf("合法 JSON 字符串应解析为对象: %v", r)
	}
	if r := normalizeToolResponse("12:00"); r.(map[string]any)["result"] != "12:00" {
		t.Fatalf("非 JSON 应包裹 result: %v", r)
	}
	if r := normalizeToolResponse([]any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "image"},
	}); r.(map[string]any)["result"] == "" {
		t.Fatalf("多 item 数组应包裹 result: %v", r)
	}
}