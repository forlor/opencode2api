package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// ParseModelSuffixes 从模型名剥离内置后缀，按 AIStudioToAPI 的"反向剥离"顺序：
//   1) -search/-code 链式（built-in tools）
//   2) -real/-fake（streaming 覆盖）
//   3) -minimal/(minimal)/-high/-low/-medium（thinkingLevel）
// 组合的用户可见后缀顺序：thinking → streaming → built-in tools。
func ParseModelSuffixes(model string) SuffixInfo {
	info := SuffixInfo{CleanModel: model}
	if model == "" {
		return info
	}

	// 循环剥离直至无可识别后缀：支持 -search/-code/-real/-fake/-thinkingLevel 任意组合顺序
	for {
		if base, ok := stripTrailingSuffix(info.CleanModel, "search"); ok {
			info.ForceWebSearch = true
			info.CleanModel = base
			continue
		}
		if base, ok := stripTrailingSuffix(info.CleanModel, "code"); ok {
			info.ForceCodeExecution = true
			info.CleanModel = base
			continue
		}
		if base, ok := stripTrailingSuffix(info.CleanModel, "real"); ok {
			info.StreamingMode = "real"
			info.HasStreamingSuffix = true
			info.CleanModel = base
			continue
		}
		if base, ok := stripTrailingSuffix(info.CleanModel, "fake"); ok {
			info.StreamingMode = "fake"
			info.HasStreamingSuffix = true
			info.CleanModel = base
			continue
		}
		if base, level, ok := parseThinkingLevelSuffix(info.CleanModel); ok {
			info.ThinkingLevel = level
			info.HasThinkingSuffix = true
			info.CleanModel = base
			continue
		}
		break
	}

	return info
}

// stripTrailingSuffix 大小写不敏感地剥离尾部 "-suffix"
func stripTrailingSuffix(model, suffix string) (string, bool) {
	if len(model) <= len(suffix)+1 {
		return model, false
	}
	if !strings.HasSuffix(strings.ToLower(model), "-"+suffix) {
		return model, false
	}
	return model[:len(model)-len(suffix)-1], true
}

// parseThinkingLevelSuffix 支持括号格式 model(minimal) 与连字符格式 model-minimal
func parseThinkingLevelSuffix(model string) (base string, level string, ok bool) {
	lower := strings.ToLower(model)
	// 括号格式
	if idx := strings.LastIndex(model, "("); idx > 0 && strings.HasSuffix(model, ")") {
		inside := lower[idx+1 : len(lower)-1]
		if lv, found := ThinkingLevelMap[inside]; found {
			return model[:idx], lv, true
		}
	}
	// 连字符格式
	if idx := strings.LastIndex(model, "-"); idx > 0 {
		suffix := lower[idx+1:]
		if lv, found := ThinkingLevelMap[suffix]; found {
			return model[:idx], lv, true
		}
	}
	return model, "", false
}

// ==================== 通用 map 访问辅助 ====================

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func asInt(v any) (int, bool) {
	if f, ok := asFloat(v); ok {
		return int(f), true
	}
	if s, ok := v.(string); ok {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// getMap 取值并按需下钻（extra_body → google → thinking_config 等）
func getMap(m map[string]any, keys ...string) (map[string]any, bool) {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[k]
		if !ok {
			return nil, false
		}
	}
	res, ok := cur.(map[string]any)
	return res, ok
}

// ==================== part 构建辅助 ====================

func textPart(s string) GeminiPart {
	return GeminiPart{Text: &s}
}

func thoughtPart(s string) GeminiPart {
	t := true
	return GeminiPart{Text: &s, Thought: &t}
}

func functionCallPart(name string, args any, sig string) GeminiPart {
	return GeminiPart{
		FunctionCall:     &GeminiFunctionCall{Name: name, Args: args},
		ThoughtSignature: sig,
	}
}

func functionResponsePart(name string, resp any) GeminiPart {
	return GeminiPart{FunctionResponse: &GeminiFunctionResponse{Name: name, Response: resp}}
}

func inlineDataPart(mime, data string) GeminiPart {
	return GeminiPart{InlineData: &GeminiInlineData{MimeType: mime, Data: data}}
}

// ==================== 工具 / Schema / 安全相关 ====================

// hasGeminiTool 判断 tools 中是否已存在指定内置工具 key
func hasGeminiTool(tools []GeminiTool, key string) bool {
	for _, t := range tools {
		switch key {
		case "googleSearch":
			if t.GoogleSearch != nil {
				return true
			}
		case "googleSearchRetrieval":
			if t.GoogleSearchRetrieval != nil {
				return true
			}
		case "codeExecution":
			if t.CodeExecution != nil {
				return true
			}
		case "urlContext":
			if t.URLContext != nil {
				return true
			}
		}
	}
	return false
}

func hasFunctionDeclarations(tools []GeminiTool) bool {
	for _, t := range tools {
		if len(t.FunctionDeclarations) > 0 {
			return true
		}
	}
	return false
}

// defaultSafetySettings 返回默认安全设置（AIStudioToAPI 一致）
func defaultSafetySettings(threshold string) []GeminiSafetySetting {
	if threshold == "" {
		threshold = "OFF"
	}
	return []GeminiSafetySetting{
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: threshold},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: threshold},
		{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: threshold},
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: threshold},
	}
}

// ensureServerSideToolInvocations 同时存在内置工具与 functionDeclarations 时启用服务端工具调用
func ensureServerSideToolInvocations(g *GeminiRequest) {
	if len(g.Tools) == 0 {
		return
	}
	hasBuiltIn := false
	for _, t := range g.Tools {
		if t.GoogleSearch != nil || t.CodeExecution != nil || t.URLContext != nil || t.GoogleSearchRetrieval != nil {
			hasBuiltIn = true
			break
		}
	}
	if !hasBuiltIn || !hasFunctionDeclarations(g.Tools) {
		return
	}
	if g.ToolConfig == nil {
		g.ToolConfig = &GeminiToolConfig{}
	}
	if g.ToolConfig.IncludeServerSideToolInvocations == nil {
		t := true
		g.ToolConfig.IncludeServerSideToolInvocations = &t
	}
}

// finalizeGeminiRequest 注入 force 工具 + 服务端工具调用 + 默认 safetySettings
func finalizeGeminiRequest(g *GeminiRequest, opts ConvertOpts, forceWeb, forceCode, forceURL bool) {
	if forceWeb || forceCode || forceURL {
		if g.Tools == nil {
			g.Tools = []GeminiTool{}
		}
		if forceWeb && !hasGeminiTool(g.Tools, "googleSearch") {
			g.Tools = append(g.Tools, GeminiTool{GoogleSearch: map[string]any{}})
		}
		if forceURL && !hasGeminiTool(g.Tools, "urlContext") {
			g.Tools = append(g.Tools, GeminiTool{URLContext: map[string]any{}})
		}
		if forceCode && !hasGeminiTool(g.Tools, "codeExecution") {
			g.Tools = append(g.Tools, GeminiTool{CodeExecution: map[string]any{}})
		}
	}
	ensureServerSideToolInvocations(g)
	g.SafetySettings = defaultSafetySettings(opts.SafetyThreshold)
}

// ==================== ID 生成 ====================

var reqIDCounter uint64

// newID 生成 request id（时间戳 + 原子计数器，避免每请求 crypto/rand 开销）
func newID(prefix string) string {
	ts := time.Now().UnixNano()
	seq := atomic.AddUint64(&reqIDCounter, 1)
	return fmt.Sprintf("%s_%x_%x", prefix, ts, seq)
}

// ==================== JSON 小工具 ====================

func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
