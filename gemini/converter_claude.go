package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// ==================== Claude → Gemini（请求侧） ====================

// TranslateClaudeToGemini 将 Anthropic /v1/messages 请求体转换为 Gemini generateContent 请求体。
// 移植自 AIStudioToAPI FormatConverter.translateClaudeToGoogle。
// 返回转换后的请求、模型后缀解析结果与错误。
func TranslateClaudeToGemini(req map[string]any, opts ConvertOpts) (GeminiRequest, SuffixInfo, error) {
	rawModel := "gemini-2.5-flash-lite"
	if m := asString(req["model"]); m != "" {
		rawModel = m
	}
	suffix := ParseModelSuffixes(rawModel)

	g := GeminiRequest{}

	// 1. system（顶层或消息内）→ systemInstruction（多段文本以 \n 合并为单 part，role user）
	var systemText string
	appendSystem := func(content any) {
		if t := claudeBlockText(content); t != "" {
			if systemText != "" {
				systemText += "\n"
			}
			systemText += t
		}
	}
	if sys, ok := req["system"]; ok {
		appendSystem(sys)
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok || asString(mm["role"]) != "system" {
				continue
			}
			appendSystem(mm["content"])
		}
	}
	if systemText != "" {
		g.SystemInstruction = &GeminiContent{Role: "user", Parts: []GeminiPart{textPart(systemText)}}
	}

	// 2. 预扫描 assistant tool_use 块，建立 tool_use_id → function 名映射
	//    （Gemini functionResponse 需要原名，Claude tool_result 只有 id）
	toolNameByID := map[string]string{}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok || asString(mm["role"]) != "assistant" {
				continue
			}
			arr, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			for _, b := range arr {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if asString(bm["type"]) == "tool_use" && asString(bm["id"]) != "" && asString(bm["name"]) != "" {
					toolNameByID[asString(bm["id"])] = asString(bm["name"])
				}
			}
		}
	}

	// 3. 对话消息 → contents
	var contents []GeminiContent
	var pendingToolParts []GeminiPart
	flushToolParts := func() {
		if len(pendingToolParts) > 0 {
			contents = append(contents, GeminiContent{Role: "user", Parts: pendingToolParts})
			pendingToolParts = nil
		}
	}

	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role := asString(mm["role"])
			if role == "system" {
				continue
			}

			var googleParts []GeminiPart

			// 3a. tool_result 消息 → functionResponse（缓冲合并，本消息其余 text/image 块一并缓冲）
			if role == "user" && hasToolResultBlocks(mm["content"]) {
				if arr, ok := mm["content"].([]any); ok {
					for _, b := range arr {
						bm, ok := b.(map[string]any)
						if !ok {
							continue
						}
						if asString(bm["type"]) != "tool_result" {
							continue
						}
						name := toolNameByID[asString(bm["tool_use_id"])]
						if name == "" {
							name = "unknown_function"
						}
						pendingToolParts = append(pendingToolParts,
							functionResponsePart(name, normalizeClaudeToolResultContent(bm["content"])))
					}
					// 同消息中非 tool_result 的内容块
					for _, b := range arr {
						bm, ok := b.(map[string]any)
						if !ok {
							continue
						}
						switch asString(bm["type"]) {
						case "tool_result":
							continue
						case "text":
							if t := asString(bm["text"]); t != "" {
								pendingToolParts = append(pendingToolParts, textPart(t))
							}
						case "image":
							pendingToolParts = append(pendingToolParts, claudeImagePart(bm, opts))
						}
					}
				}
				continue // 与 AIStudioToAPI 一致：tool_result 消息不产生独立 content
			}

			// 非 tool_result 消息前 flush
			flushToolParts()

			// 3b. assistant 块：tool_use→functionCall（thoughtSignature 只在首个）、thinking→thought、text
			if role == "assistant" {
				if arr, ok := mm["content"].([]any); ok {
					sigAttached := false
					for _, b := range arr {
						bm, ok := b.(map[string]any)
						if !ok {
							continue
						}
						switch asString(bm["type"]) {
						case "tool_use":
							args := bm["input"]
							if args == nil {
								args = map[string]any{}
							}
							sig := ""
							if !sigAttached {
								sig = DummyThoughtSignature
								sigAttached = true
							}
							googleParts = append(googleParts, functionCallPart(asString(bm["name"]), args, sig))
						case "thinking":
							if t := asString(bm["thinking"]); t != "" {
								googleParts = append(googleParts, thoughtPart(t))
							}
						case "text":
							if t := asString(bm["text"]); t != "" {
								googleParts = append(googleParts, textPart(t))
							}
						}
					}
				}
			}

			// 3c. 普通文本 / 图片内容
			if len(googleParts) == 0 {
				switch content := mm["content"].(type) {
				case string:
					if content != "" {
						googleParts = append(googleParts, textPart(content))
					}
				case []any:
					for _, b := range content {
						bm, ok := b.(map[string]any)
						if !ok {
							continue
						}
						switch asString(bm["type"]) {
						case "text":
							if t := asString(bm["text"]); t != "" {
								googleParts = append(googleParts, textPart(t))
							}
						case "image":
							googleParts = append(googleParts, claudeImagePart(bm, opts))
						}
					}
				}
			}

			if len(googleParts) > 0 {
				grole := "user"
				if role == "assistant" {
					grole = "model"
				}
				contents = append(contents, GeminiContent{Role: grole, Parts: googleParts})
			}
		}
	}
	flushToolParts()
	g.Contents = contents

	// 4. generationConfig
	gc := &GeminiGenerationConfig{}
	if v, ok := asInt(req["max_tokens"]); ok {
		gc.MaxOutputTokens = v
	}
	if v, ok := req["stop_sequences"]; ok {
		gc.StopSequences = normalizeStopSequences(v)
	}
	if v, ok := asFloat(req["temperature"]); ok {
		f := v
		gc.Temperature = &f
	}
	if v, ok := asFloat(req["top_k"]); ok {
		f := v
		gc.TopK = &f
	}
	if v, ok := asFloat(req["top_p"]); ok {
		f := v
		gc.TopP = &f
	}
	gc.ThinkingConfig = claudeThinkingConfig(req, opts, suffix.ThinkingLevel)

	// 5. output_format / output_config → responseSchema
	applyClaudeResponseFormat(req, gc)
	g.GenerationConfig = gc

	// 6. tools（Claude 自定义 + 内置 web_search/web_fetch/code_execution）→ functionDeclarations
	forceWeb, forceCode := suffix.ForceWebSearch, suffix.ForceCodeExecution
	forceURL := false // urlContext 无模型后缀，仅来自 web_fetch 工具映射
	if tools, ok := req["tools"].([]any); ok {
		var decls []GeminiFunctionDeclaration
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			ttype := asString(tm["type"])
			name := asString(tm["name"])
			switch {
			case strings.HasPrefix(ttype, "web_search_") && name == "web_search":
				forceWeb = true
				continue
			case strings.HasPrefix(ttype, "web_fetch_") && name == "web_fetch":
				forceURL = true
				continue
			case strings.HasPrefix(ttype, "code_execution_") && name == "code_execution":
				forceCode = true
				continue
			}
			if name == "" {
				continue
			}
			d := GeminiFunctionDeclaration{Name: name}
			if desc := asString(tm["description"]); desc != "" {
				d.Description = desc
			}
			if schema, ok := tm["input_schema"]; ok {
				d.Parameters = ConvertSchemaToGemini(schema, false, false)
			}
			decls = append(decls, d)
		}
		if len(decls) > 0 {
			g.Tools = append(g.Tools, GeminiTool{FunctionDeclarations: decls})
		}
	}

	// 7. tool_choice → toolConfig
	applyClaudeToolChoice(req, &g)

	// 8. 收尾：force 工具 + 服务端工具调用 + safetySettings
	finalizeGeminiRequest(&g, opts, forceWeb, forceCode, forceURL)

	return g, suffix, nil
}

// hasToolResultBlocks 判断消息内容是否含 tool_result 块
func hasToolResultBlocks(content any) bool {
	arr, ok := content.([]any)
	if !ok {
		return false
	}
	for _, b := range arr {
		if bm, ok := b.(map[string]any); ok && asString(bm["type"]) == "tool_result" {
			return true
		}
	}
	return false
}

// claudeBlockText 提取 Claude system/text 内容为纯文本
func claudeBlockText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if s, ok := b.(string); ok {
				parts = append(parts, s)
				continue
			}
			if m, ok := b.(map[string]any); ok {
				if t := asString(m["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		return asString(c["text"])
	}
	return ""
}

// normalizeClaudeToolResultContent Claude tool_result.content → Gemini functionResponse.response（object）
func normalizeClaudeToolResultContent(content any) any {
	switch c := content.(type) {
	case string:
		if parsed := tryParseJSON(c); parsed != nil {
			return ensureResponseObject(parsed)
		}
		return map[string]any{"result": c}
	case []any:
		var textParts []string
		for _, item := range c {
			if im, ok := item.(map[string]any); ok && asString(im["type"]) == "text" {
				if t := asString(im["text"]); t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			joined := strings.Join(textParts, "\n")
			if parsed := tryParseJSON(joined); parsed != nil {
				return ensureResponseObject(parsed)
			}
			return map[string]any{"result": joined}
		}
		return map[string]any{"result": c}
	default:
		if content == nil {
			return map[string]any{"result": ""}
		}
		return ensureResponseObject(content)
	}
}

func ensureResponseObject(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": v}
}

func tryParseJSON(s string) any {
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err == nil {
		return parsed
	}
	return nil
}

// claudeImagePart Claude image 块 → Gemini inlineData（base64 直解 / url 下载）
func claudeImagePart(bm map[string]any, opts ConvertOpts) GeminiPart {
	src, ok := bm["source"].(map[string]any)
	if !ok {
		return textPart("[System Note: Skipped an image input because image source was missing]")
	}
	switch asString(src["type"]) {
	case "base64":
		return inlineDataPart(asString(src["media_type"]), asString(src["data"]))
	case "url":
		url := asString(src["url"])
		if opts.HTTPImageClient == nil {
			return textPart("[System Note: Skipped an image input because image URL download is disabled]")
		}
		mime, data, err := downloadImage(context.Background(), url, opts.HTTPImageClient)
		if err != nil {
			return textPart("[System Note: Failed to load image from " + url + "]")
		}
		return inlineDataPart(mime, data)
	}
	return textPart("[System Note: Skipped an image input because image source format was unsupported]")
}

// claudeThinkingConfig 解析 Claude thinking（顶层或 metadata.thinking）→ Gemini thinkingConfig
func claudeThinkingConfig(req map[string]any, opts ConvertOpts, modelLevel string) *GeminiThinkingConfig {
	var tc *GeminiThinkingConfig
	thinkingParam := req["thinking"]
	if thinkingParam == nil {
		if meta, ok := req["metadata"].(map[string]any); ok {
			thinkingParam = meta["thinking"]
		}
	}
	if tp, ok := thinkingParam.(map[string]any); ok {
		if asBool(tp["enabled"]) || asString(tp["type"]) == "enabled" {
			tc = &GeminiThinkingConfig{IncludeThoughts: boolPtr(true)}
		}
	}
	if opts.ForceThinking && (tc == nil || tc.IncludeThoughts == nil) {
		if tc == nil {
			tc = &GeminiThinkingConfig{}
		}
		tc.IncludeThoughts = boolPtr(true)
	}
	if modelLevel != "" {
		if tc == nil {
			tc = &GeminiThinkingConfig{}
		}
		tc.ThinkingLevel = modelLevel
	}
	return tc
}

// applyClaudeResponseFormat Claude output_format / output_config → responseSchema
func applyClaudeResponseFormat(req map[string]any, gc *GeminiGenerationConfig) {
	if of, ok := req["output_format"].(map[string]any); ok {
		switch asString(of["type"]) {
		case "json_schema":
			schema := of["schema"]
			if schema == nil {
				if js, ok := of["json_schema"].(map[string]any); ok {
					schema = js["schema"]
				}
			}
			if schema != nil {
				gc.ResponseMimeType = "application/json"
				gc.ResponseSchema = ConvertSchemaToGemini(schema, true, false)
			}
		case "json_object":
			gc.ResponseMimeType = "application/json"
		case "text":
			gc.ResponseMimeType = "text/plain"
		}
	}
	if oc, ok := req["output_config"].(map[string]any); ok {
		if f, ok := oc["format"].(map[string]any); ok {
			if asString(f["type"]) == "json_schema" {
				if schema, ok := f["schema"]; ok {
					gc.ResponseMimeType = "application/json"
					gc.ResponseSchema = ConvertSchemaToGemini(schema, true, false)
				}
			}
		}
	}
}

// applyClaudeToolChoice Claude tool_choice → Gemini toolConfig（需存在 functionDeclarations）
func applyClaudeToolChoice(req map[string]any, g *GeminiRequest) {
	tc, ok := req["tool_choice"].(map[string]any)
	if !ok || !hasFunctionDeclarations(g.Tools) {
		return
	}
	ttype := asString(tc["type"])
	name := asString(tc["name"])
	isBuiltIn := ttype == "tool" && (name == "web_search" || name == "web_fetch" || name == "code_execution")

	cfg := GeminiFunctionCallingConfig{}
	switch ttype {
	case "auto":
		cfg.Mode = "AUTO"
	case "none":
		cfg.Mode = "NONE"
	case "any":
		cfg.Mode = "ANY"
	case "tool":
		if name != "" && !isBuiltIn {
			cfg.Mode = "ANY"
			cfg.AllowedFunctionNames = []string{name}
		}
	}
	if cfg.Mode != "" {
		g.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: &cfg}
	}
}

// ==================== Gemini → Claude（响应侧） ====================

// TranslateGeminiToClaude Gemini 非流式响应 → Anthropic message
func TranslateGeminiToClaude(resp *GeminiResponse, model string) map[string]any {
	usage := ParseUsage(resp.UsageMetadata)
	msgID := "msg_" + newID("msg")
	candidate := candidate0(*resp)

	buildMessage := func(content []any, stopReason string) map[string]any {
		return map[string]any{
			"type":          "message",
			"role":          "assistant",
			"id":            msgID,
			"model":         model,
			"content":       content,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  usage["prompt_tokens"],
				"output_tokens": usage["completion_tokens"],
			},
		}
	}

	if candidate == nil {
		return buildMessage([]any{map[string]any{"type": "text", "text": ""}}, "end_turn")
	}

	var content []any
	hasToolUse := false
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			switch {
			case part.Thought != nil && *part.Thought && part.Text != nil:
				sig := part.ThoughtSignature
				if sig == "" {
					sig = DummyThoughtSignature
				}
				content = append(content, map[string]any{
					"type":      "thinking",
					"thinking":  *part.Text,
					"signature": sig,
				})
			case part.Text != nil:
				content = append(content, map[string]any{"type": "text", "text": *part.Text})
			case part.InlineData != nil:
				content = append(content, map[string]any{
					"type": "text",
					"text": "![Generated Image](data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data + ")",
				})
			case part.FunctionCall != nil:
				hasToolUse = true
				input := map[string]any{}
				if part.FunctionCall.Args != nil {
					input = asMap(part.FunctionCall.Args)
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    "toolu_" + newID("toolu"),
					"name":  part.FunctionCall.Name,
					"input": input,
				})
			}
		}
	}

	stopReason := "end_turn"
	switch {
	case hasToolUse:
		stopReason = "tool_use"
	case candidate.FinishReason == "MAX_TOKENS":
		stopReason = "max_tokens"
	}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}
	return buildMessage(content, stopReason)
}

// TranslateGeminiToClaudeStream Gemini 流式 SSE 单事件 → Claude SSE 帧（event:/data: 格式）。
// 帧内 JSON 的 type 字段与 event: 行重复，与 AIStudioToAPI 输出逐字节一致。
func TranslateGeminiToClaudeStream(raw []byte, model string, st *ClaudeStreamState) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, nil
	}
	var resp GeminiResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, nil
	}

	if resp.UsageMetadata != nil {
		input := resp.UsageMetadata.PromptTokenCount + resp.UsageMetadata.ToolUsePromptTokenCount
		output := resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount
		if input > 0 {
			st.InputTokens = input
		}
		st.OutputTokens = output
	}
	if st.MessageID == "" {
		st.MessageID = "msg_" + newID("msg")
	}

	candidate := candidate0(resp)
	if candidate == nil {
		return nil, nil
	}

	var out []byte
	push := func(event string, data map[string]any) {
		out = append(out, "event: "+event+"\n"...)
		out = append(out, "data: "+string(marshalJSON(data))+"\n\n"...)
	}

	if !st.MessageStartSent {
		push("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"type":          "message",
				"id":            st.MessageID,
				"role":          "assistant",
				"model":         model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": st.InputTokens, "output_tokens": 0},
			},
		})
		st.MessageStartSent = true
	}

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			switch {
			case part.Thought != nil && *part.Thought && part.Text != nil:
				if !st.ThinkingBlockStarted {
					push("content_block_start", map[string]any{
						"type":          "content_block_start",
						"index":         st.ContentBlockIndex,
						"content_block": map[string]any{"type": "thinking", "thinking": ""},
					})
					st.ThinkingBlockStarted = true
					st.ThinkingBlockIndex = st.ContentBlockIndex
					st.ContentBlockIndex++
				}
				push("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.ThinkingBlockIndex,
					"delta": map[string]any{"type": "thinking_delta", "thinking": *part.Text},
				})
			case part.Text != nil:
				if st.ThinkingBlockStarted && !st.ThinkingBlockStopped {
					push("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.ThinkingBlockIndex})
					st.ThinkingBlockStopped = true
				}
				if !st.TextBlockStarted {
					push("content_block_start", map[string]any{
						"type":          "content_block_start",
						"index":         st.ContentBlockIndex,
						"content_block": map[string]any{"type": "text", "text": ""},
					})
					st.TextBlockStarted = true
					st.TextBlockIndex = st.ContentBlockIndex
					st.ContentBlockIndex++
				}
				push("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.TextBlockIndex,
					"delta": map[string]any{"type": "text_delta", "text": *part.Text},
				})
			case part.InlineData != nil:
				if st.ThinkingBlockStarted && !st.ThinkingBlockStopped {
					push("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.ThinkingBlockIndex})
					st.ThinkingBlockStopped = true
				}
				if !st.TextBlockStarted {
					push("content_block_start", map[string]any{
						"type":          "content_block_start",
						"index":         st.ContentBlockIndex,
						"content_block": map[string]any{"type": "text", "text": ""},
					})
					st.TextBlockStarted = true
					st.TextBlockIndex = st.ContentBlockIndex
					st.ContentBlockIndex++
				}
				md := "![Generated Image](data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data + ")"
				push("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.TextBlockIndex,
					"delta": map[string]any{"type": "text_delta", "text": md},
				})
			case part.FunctionCall != nil:
				input := map[string]any{}
				if part.FunctionCall.Args != nil {
					input = asMap(part.FunctionCall.Args)
				}
				push("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.ContentBlockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    "toolu_" + newID("toolu"),
						"name":  part.FunctionCall.Name,
						"input": map[string]any{},
					},
				})
				push("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.ContentBlockIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": string(marshalJSON(input))},
				})
				push("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.ContentBlockIndex})
				st.ContentBlockIndex++
				st.HasToolUse = true
			}
		}
	}

	if candidate.FinishReason != "" {
		if st.TextBlockStarted && !st.TextBlockStopped {
			push("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.TextBlockIndex})
			st.TextBlockStopped = true
		}
		if st.ThinkingBlockStarted && !st.ThinkingBlockStopped {
			push("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.ThinkingBlockIndex})
			st.ThinkingBlockStopped = true
		}
		stopReason := "end_turn"
		if st.HasToolUse {
			stopReason = "tool_use"
		} else if candidate.FinishReason == "MAX_TOKENS" {
			stopReason = "max_tokens"
		}
		push("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": st.OutputTokens},
		})
		push("message_stop", map[string]any{"type": "message_stop"})
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// asMap 将 any 安全转换为 map[string]any
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}