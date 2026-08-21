package gemini

import (
	"encoding/json"
	"testing"
)

// 无后缀无开关：body 字节级不变（透传零开销路径）
func TestInjectNativeTweaks_NoChange(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":512},"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}],"cachedContent":"xyz"}`)
	out := InjectNativeTweaks(body, ParseModelSuffixes("gemini-2.5-flash"), ConvertOpts{})
	if string(out) != string(body) {
		t.Fatalf("无注入时应原样返回:\n got %s\nwant %s", out, body)
	}
}

// 全量注入：-search 后缀 + thinkingLevel + maxOutputTokens 兜底 + safetySettings
func TestInjectNativeTweaks_All(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	suffix := ParseModelSuffixes("gemini-2.5-flash-search-minimal")
	if !suffix.ForceWebSearch || suffix.ThinkingLevel != "MINIMAL" {
		t.Fatalf("后缀解析异常: %+v", suffix)
	}
	out := InjectNativeTweaks(body, suffix, ConvertOpts{
		SafetyThreshold:         "BLOCK_ONLY_HIGH",
		MaxOutputTokensFallback: 4096,
	})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	// googleSearch 工具
	tools := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["googleSearch"]; !ok {
		t.Fatalf("应注入 googleSearch: %v", tools)
	}
	// thinkingLevel + maxOutputTokens
	gc := m["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(4096) {
		t.Fatalf("maxOutputTokens = %v", gc["maxOutputTokens"])
	}
	if gc["thinkingConfig"].(map[string]any)["thinkingLevel"] != "MINIMAL" {
		t.Fatalf("thinkingConfig = %v", gc["thinkingConfig"])
	}
	// safetySettings
	ss := m["safetySettings"].([]any)
	if len(ss) != 4 || ss[0].(map[string]any)["threshold"] != "BLOCK_ONLY_HIGH" {
		t.Fatalf("safetySettings = %v", ss)
	}
	// 原有内容保留
	if _, ok := m["contents"]; !ok {
		t.Fatalf("contents 丢失: %s", out)
	}
}

// 客户端已写字段不覆盖：maxOutputTokens/thinkingLevel/safetySettings/tools 均尊重原值
func TestInjectNativeTweaks_RespectExisting(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"foo"}]}],
		"generationConfig":{"maxOutputTokens":100,"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}},
		"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]
	}`)
	suffix := ParseModelSuffixes("gemini-2.5-flash-search-low")
	out := InjectNativeTweaks(body, suffix, ConvertOpts{
		SafetyThreshold:         "BLOCK_ONLY_HIGH",
		MaxOutputTokensFallback: 4096,
	})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	// 已有 maxOutputTokens → 不覆盖
	if m["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(100) {
		t.Fatalf("maxOutputTokens 不应覆盖: %v", m["generationConfig"])
	}
	// 已有 thinkingLevel → 不覆盖；includeThoughts 保留
	tc := m["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "HIGH" || tc["includeThoughts"] != true {
		t.Fatalf("thinkingConfig 不应覆盖: %v", tc)
	}
	// 已有 safetySettings → 不注入
	if len(m["safetySettings"].([]any)) != 1 {
		t.Fatalf("safetySettings 不应覆盖: %v", m["safetySettings"])
	}
	// 已有 tools → 追加 googleSearch 不动 functionDeclarations
	tools := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools 应追加不替换: %v", tools)
	}
	if len(tools[0].(map[string]any)["functionDeclarations"].([]any)) != 1 {
		t.Fatalf("原 tools 应保留: %v", tools[0])
	}
	if _, ok := tools[1].(map[string]any)["googleSearch"]; !ok {
		t.Fatalf("应追加 googleSearch: %v", tools)
	}
}

// 线级 force 开关：ForceUrlContext/ForceCodeExecution（无后缀也注入）
func TestInjectNativeTweaks_LineForce(t *testing.T) {
	body := []byte(`{"contents":[]}`)
	out := InjectNativeTweaks(body, ParseModelSuffixes("gemini-2.5-flash"), ConvertOpts{
		ForceUrlContext:    true,
		ForceCodeExecution: true,
	})
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	tools := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["urlContext"]; !ok {
		t.Fatalf("应注入 urlContext: %v", tools)
	}
	if _, ok := tools[1].(map[string]any)["codeExecution"]; !ok {
		t.Fatalf("应注入 codeExecution: %v", tools)
	}
}

// 非法 JSON：原样返回
func TestInjectNativeTweaks_InvalidJSON(t *testing.T) {
	body := []byte(`{not json`)
	out := InjectNativeTweaks(body, ParseModelSuffixes("m-search"), ConvertOpts{ForceWebSearch: true})
	if string(out) != string(body) {
		t.Fatalf("非法 JSON 应原样返回: %s", out)
	}
}