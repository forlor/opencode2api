package gemini

import "encoding/json"

// InjectNativeTweaks 原生 /v1beta/ 入口的请求体微调（map 级 JSON 操作，未知字段原样保留）：
//  1. -search/-code 模型后缀与线级 force 开关 → 注入 googleSearch/urlContext/codeExecution 内置工具
//  2. thinkingLevel 模型后缀 → generationConfig.thinkingConfig.thinkingLevel（已有值不覆盖）
//  3. maxOutputTokens 兜底（Gemini 3 缺省会 400）
//  4. safetySettings 注入（客户端已写则不覆盖）
//
// 解析失败的 body 原样返回，由上游报 400。
func InjectNativeTweaks(body []byte, suffix SuffixInfo, opts ConvertOpts) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	changed := false

	// 1) 内置工具注入（不与请求中已有工具重复）
	forceWeb := suffix.ForceWebSearch || opts.ForceWebSearch
	forceCode := suffix.ForceCodeExecution || opts.ForceCodeExecution
	forceURL := opts.ForceUrlContext
	if forceWeb || forceCode || forceURL {
		tools, _ := m["tools"].([]any)
		has := func(key string) bool {
			for _, t := range tools {
				if tm, ok := t.(map[string]any); ok {
					if _, ok := tm[key]; ok {
						return true
					}
				}
			}
			return false
		}
		appendTool := func(key string) {
			if !has(key) {
				tools = append(tools, map[string]any{key: map[string]any{}})
				changed = true
			}
		}
		if forceWeb {
			appendTool("googleSearch")
		}
		if forceURL {
			appendTool("urlContext")
		}
		if forceCode {
			appendTool("codeExecution")
		}
		if changed {
			m["tools"] = tools
		}
	}

	// 2) thinkingLevel 后缀 → thinkingConfig（map 级下钻，保留其余 thinking 字段）
	if suffix.ThinkingLevel != "" {
		gc, _ := m["generationConfig"].(map[string]any)
		if gc == nil {
			gc = map[string]any{}
		}
		tc, _ := gc["thinkingConfig"].(map[string]any)
		if tc == nil {
			tc = map[string]any{}
		}
		if asString(tc["thinkingLevel"]) == "" {
			tc["thinkingLevel"] = suffix.ThinkingLevel
			gc["thinkingConfig"] = tc
			m["generationConfig"] = gc
			changed = true
		}
	}

	// 3) maxOutputTokens 兜底
	if opts.MaxOutputTokensFallback > 0 {
		gc, _ := m["generationConfig"].(map[string]any)
		if gc == nil {
			gc = map[string]any{}
		}
		if v, ok := asInt(gc["maxOutputTokens"]); !ok || v == 0 {
			gc["maxOutputTokens"] = opts.MaxOutputTokensFallback
			m["generationConfig"] = gc
			changed = true
		}
	}

	// 4) safetySettings（客户端已写则尊重）
	if opts.SafetyThreshold != "" {
		if _, ok := m["safetySettings"].([]any); !ok {
			ss := defaultSafetySettings(opts.SafetyThreshold)
			arr := make([]any, len(ss))
			for i, s := range ss {
				arr[i] = map[string]any{"category": s.Category, "threshold": s.Threshold}
			}
			m["safetySettings"] = arr
			changed = true
		}
	}

	if !changed {
		return body
	}
	return marshalJSON(m)
}