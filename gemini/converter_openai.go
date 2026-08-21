package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var dataURLPattern = regexp.MustCompile(`^data:(image/[^;]+);base64,(.+)$`)

func boolPtr(b bool) *bool { return &b }

// ==================== OpenAI → Gemini（请求侧） ====================

// TranslateOpenAIToGemini 将 OpenAI /v1/chat/completions 请求体转换为 Gemini generateContent 请求体。
// 移植自 AIStudioToAPI FormatConverter.translateOpenAIToGoogle。
// 返回转换后的请求、模型后缀解析结果（含 cleanModel / streamingMode）与错误。
func TranslateOpenAIToGemini(req map[string]any, opts ConvertOpts) (GeminiRequest, SuffixInfo, error) {
	rawModel := "gemini-2.5-flash-lite"
	if m := asString(req["model"]); m != "" {
		rawModel = m
	}
	suffix := ParseModelSuffixes(rawModel)

	g := GeminiRequest{}

	// 1. system 消息 → systemInstruction
	var systemParts []GeminiPart
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok || asString(mm["role"]) != "system" {
				continue
			}
			if s := asString(mm["content"]); s != "" {
				systemParts = append(systemParts, textPart(s))
			}
		}
	}
	if len(systemParts) > 0 {
		// 与 AIStudioToAPI 一致：systemInstruction 的 role 用 "user"
		g.SystemInstruction = &GeminiContent{Role: "user", Parts: systemParts}
	}

	// 2. 对话消息 → contents
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

			// 2a. tool 角色 → functionResponse（缓冲，合并连续 tool 消息为单 user）
			if role == "tool" {
				name := asString(mm["name"])
				if name == "" {
					name = "unknown_function"
				}
				pendingToolParts = append(pendingToolParts,
					functionResponsePart(name, normalizeToolResponse(mm["content"])))
				continue
			}

			// 非 tool 消息前 flush
			flushToolParts()

			var parts []GeminiPart

			// 2b. assistant tool_calls → functionCall（thoughtSignature 只在首个 functionCall）
			if role == "assistant" {
				if tcs, ok := mm["tool_calls"].([]any); ok {
					sigAttached := false
					for _, tc := range tcs {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						if asString(tcm["type"]) != "function" {
							continue
						}
						fn, ok := tcm["function"].(map[string]any)
						if !ok {
							continue
						}
						name := asString(fn["name"])
						if name == "" {
							continue
						}
						args := parseToolCallArgs(fn["arguments"])
						sig := ""
						if !sigAttached {
							sig = DummyThoughtSignature
							sigAttached = true
						}
						parts = append(parts, functionCallPart(name, args, sig))
					}
				}
			}

			// 2c. 普通文本 / 图片内容
			switch content := mm["content"].(type) {
			case string:
				if content != "" {
					parts = append(parts, textPart(content))
				}
			case []any:
				for _, p := range content {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					switch asString(pm["type"]) {
					case "text":
						if t := asString(pm["text"]); t != "" {
							parts = append(parts, textPart(t))
						}
					case "image_url":
						if part, handled := handleOpenAIImageURL(pm["image_url"], opts); handled {
							parts = append(parts, part)
						}
					}
				}
			}

			if len(parts) > 0 {
				grole := "user"
				if role == "assistant" {
					grole = "model"
				}
				contents = append(contents, GeminiContent{Role: grole, Parts: parts})
			}
		}
	}
	flushToolParts()
	g.Contents = contents

	// 3. generationConfig
	gc := &GeminiGenerationConfig{}
	if v, ok := asInt(req["max_tokens"]); ok {
		gc.MaxOutputTokens = v
	}
	if stop, ok := req["stop"]; ok {
		gc.StopSequences = normalizeStopSequences(stop)
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
	gc.ThinkingConfig = extractThinkingConfig(req, opts, suffix.ThinkingLevel)
	g.GenerationConfig = gc

	// 4. tools → functionDeclarations
	if tools := convertOpenAITools(req); len(tools) > 0 {
		g.Tools = tools
	}

	// 5. tool_choice → toolConfig
	if tc := convertOpenAIToolChoice(req, hasFunctionDeclarations(g.Tools)); tc != nil {
		g.ToolConfig = tc
	}

	// 6. response_format → responseSchema / responseMimeType
	applyOpenAIResponseFormat(req, gc)

	// 7. 收尾：force 工具 + 服务端工具调用 + safetySettings
	finalizeGeminiRequest(&g, opts, suffix.ForceWebSearch, suffix.ForceCodeExecution, false)

	return g, suffix, nil
}

// parseToolCallArgs 解析 OpenAI tool_calls 的 arguments（字符串 JSON 或对象）
func parseToolCallArgs(v any) any {
	if s, ok := v.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
		return map[string]any{}
	}
	if v == nil {
		return map[string]any{}
	}
	return v
}

// normalizeToolResponse 将 OpenAI tool 消息 content 规范化为 Gemini functionResponse.response（object）
func normalizeToolResponse(content any) any {
	if s, ok := content.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return normalizeToolResponse(parsed)
		}
		return map[string]any{"result": s}
	}
	if arr, ok := content.([]any); ok {
		processed := make([]any, 0, len(arr))
		for _, item := range arr {
			im, ok := item.(map[string]any)
			if !ok {
				processed = append(processed, item)
				continue
			}
			if asString(im["type"]) == "text" {
				t := asString(im["text"])
				var parsed any
				if err := json.Unmarshal([]byte(t), &parsed); err == nil {
					if pm, ok := parsed.(map[string]any); ok {
						processed = append(processed, pm)
						continue
					}
				}
				processed = append(processed, map[string]any{"content": t, "type": "text"})
				continue
			}
			processed = append(processed, item)
		}
		if len(processed) == 0 {
			return map[string]any{"result": "[]"}
		}
		if len(processed) == 1 {
			if m, ok := processed[0].(map[string]any); ok {
				return m
			}
		}
		b, _ := json.Marshal(processed)
		return map[string]any{"result": string(b)}
	}
	if m, ok := content.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": fmt.Sprint(content)}
}

// handleOpenAIImageURL 处理 image_url part（data-url / http(s) 下载）
func handleOpenAIImageURL(value any, opts ConvertOpts) (GeminiPart, bool) {
	var url string
	switch t := value.(type) {
	case string:
		url = t
	case map[string]any:
		url = asString(t["url"])
	}
	if url == "" {
		return textPart("[System Note: Skipped an image input because image_url was not a string URL]"), true
	}
	if m := dataURLPattern.FindStringSubmatch(url); m != nil {
		return inlineDataPart(m[1], m[2]), true
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		if opts.HTTPImageClient != nil {
			mime, data, err := downloadImage(context.Background(), url, opts.HTTPImageClient)
			if err == nil {
				return inlineDataPart(mime, data), true
			}
			return textPart("[System Note: Failed to load image from " + url + "]"), true
		}
		return textPart("[System Note: Skipped an image input because image URL download is disabled]"), true
	}
	return textPart("[System Note: Skipped an image input because image_url format was unsupported]"), true
}

// downloadImage 下载图片并转 base64（Phase 2 完整接通）
func downloadImage(ctx context.Context, url string, client *http.Client) (mime, data string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("image download status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", "", err
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" || strings.HasPrefix(mimeType, "application/octet-stream") {
		mimeType = "image/jpeg"
	}
	return mimeType, base64.StdEncoding.EncodeToString(body), nil
}

// normalizeStopSequences 将 stop 参数规范化为字符串数组
func normalizeStopSequences(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case []any:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if ss := asString(s); ss != "" {
				out = append(out, ss)
			}
		}
		return out
	}
	return nil
}

// extractThinkingConfig 提取 thinkingConfig（extra_body.google.thinking_config / reasoning_effort / forceThinking / 模型后缀）
func extractThinkingConfig(req map[string]any, opts ConvertOpts, modelThinkingLevel string) *GeminiThinkingConfig {
	var tc *GeminiThinkingConfig
	var raw map[string]any

	if extra, ok := req["extra_body"].(map[string]any); ok {
		if g, ok := extra["google"].(map[string]any); ok {
			if m, ok := g["thinking_config"].(map[string]any); ok {
				raw = m
			} else if m, ok := g["thinkingConfig"].(map[string]any); ok {
				raw = m
			}
		}
		if m, ok := extra["thinkingConfig"].(map[string]any); ok {
			raw = m
		} else if m, ok := extra["thinking_config"].(map[string]any); ok {
			raw = m
		}
	}
	if m, ok := req["thinkingConfig"].(map[string]any); ok {
		raw = m
	} else if m, ok := req["thinking_config"].(map[string]any); ok {
		raw = m
	}

	if raw != nil {
		tc = &GeminiThinkingConfig{}
		if v, ok := raw["include_thoughts"]; ok {
			b := asBool(v)
			tc.IncludeThoughts = &b
		} else if v, ok := raw["includeThoughts"]; ok {
			b := asBool(v)
			tc.IncludeThoughts = &b
		}
	}

	if tc == nil {
		effort := req["reasoning_effort"]
		if effort == nil {
			if extra, ok := req["extra_body"].(map[string]any); ok {
				effort = extra["reasoning_effort"]
			}
		}
		if asString(effort) != "" {
			tc = &GeminiThinkingConfig{IncludeThoughts: boolPtr(true)}
		}
	}

	if opts.ForceThinking && (tc == nil || tc.IncludeThoughts == nil) {
		if tc == nil {
			tc = &GeminiThinkingConfig{}
		}
		tc.IncludeThoughts = boolPtr(true)
	}
	if modelThinkingLevel != "" {
		if tc == nil {
			tc = &GeminiThinkingConfig{}
		}
		tc.ThinkingLevel = modelThinkingLevel
	}
	return tc
}

// convertOpenAITools OpenAI tools/functions → Gemini functionDeclarations
func convertOpenAITools(req map[string]any) []GeminiTool {
	tools := req["tools"]
	if tools == nil {
		tools = req["functions"]
	}
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	var declarations []GeminiFunctionDeclaration
	for _, t := range arr {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fd := tm["function"]
		if fd == nil {
			fd = tm // legacy functions 格式
		}
		fm, ok := fd.(map[string]any)
		if !ok {
			continue
		}
		name := asString(fm["name"])
		if name == "" {
			continue
		}
		d := GeminiFunctionDeclaration{Name: name}
		if desc := asString(fm["description"]); desc != "" {
			d.Description = desc
		}
		if params, ok := fm["parameters"]; ok {
			d.Parameters = ConvertSchemaToGemini(params, false, false)
		}
		declarations = append(declarations, d)
	}
	if len(declarations) == 0 {
		return nil
	}
	return []GeminiTool{{FunctionDeclarations: declarations}}
}

// convertOpenAIToolChoice OpenAI tool_choice → Gemini toolConfig.functionCallingConfig
func convertOpenAIToolChoice(req map[string]any, hasFuncs bool) *GeminiToolConfig {
	if !hasFuncs {
		return nil
	}
	tc := req["tool_choice"]
	if tc == nil {
		tc = req["function_call"]
	}
	if tc == nil {
		return nil
	}
	cfg := GeminiFunctionCallingConfig{}
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			cfg.Mode = "AUTO"
		case "none":
			cfg.Mode = "NONE"
		case "required":
			cfg.Mode = "ANY"
		}
	case map[string]any:
		funcName := ""
		if fn, ok := v["function"].(map[string]any); ok {
			funcName = asString(fn["name"])
		}
		if funcName == "" {
			funcName = asString(v["name"])
		}
		if funcName != "" {
			cfg.Mode = "ANY"
			cfg.AllowedFunctionNames = []string{funcName}
		}
	}
	if cfg.Mode == "" {
		return nil
	}
	return &GeminiToolConfig{FunctionCallingConfig: &cfg}
}

// applyOpenAIResponseFormat OpenAI response_format → generationConfig（structured output）
func applyOpenAIResponseFormat(req map[string]any, gc *GeminiGenerationConfig) {
	rf, ok := req["response_format"].(map[string]any)
	if !ok {
		return
	}
	switch asString(rf["type"]) {
	case "json_schema":
		js, ok := rf["json_schema"].(map[string]any)
		if !ok {
			return
		}
		schema, ok := js["schema"]
		if !ok {
			return
		}
		gc.ResponseMimeType = "application/json"
		gc.ResponseSchema = ConvertSchemaToGemini(schema, true, false)
	case "json_object":
		gc.ResponseMimeType = "application/json"
	case "text":
		// 默认行为，无需处理
	default:
		// 不支持的 response_format，忽略
	}
}

// ==================== Gemini → OpenAI（响应侧） ====================

func candidate0(resp GeminiResponse) *GeminiCandidate {
	if len(resp.Candidates) > 0 {
		return &resp.Candidates[0]
	}
	return nil
}

// mapFinishReason Gemini finishReason → OpenAI finish_reason
func mapFinishReason(geminiReason string) string {
	switch strings.ToLower(strings.TrimSpace(geminiReason)) {
	case "max_tokens":
		return "length"
	case "safety":
		return "content_filter"
	case "recitation", "other", "stop", "":
		return "stop"
	default:
		return "stop"
	}
}

// ParseUsage Gemini usageMetadata → OpenAI usage（含 reasoning/image token 明细，空指针兜底）
func ParseUsage(usage *GeminiUsageMetadata) map[string]any {
	var input, tool, ctext, reasoning, image, total int64
	if usage != nil {
		input = usage.PromptTokenCount
		tool = usage.ToolUsePromptTokenCount
		ctext = usage.CandidatesTokenCount
		reasoning = usage.ThoughtsTokenCount
		total = usage.TotalTokenCount
		for _, d := range usage.CandidatesTokensDetails {
			if d.Modality == "IMAGE" {
				image += d.TokenCount
			}
		}
	}
	prompt := input + tool
	completion := ctext + reasoning
	return map[string]any{
		"completion_tokens": completion,
		"completion_tokens_details": map[string]any{
			"image_tokens":         image,
			"output_text_tokens":   ctext,
			"reasoning_tokens":     reasoning,
		},
		"prompt_tokens": prompt,
		"prompt_tokens_details": map[string]any{
			"text_tokens": input,
			"tool_tokens": tool,
		},
		"total_tokens": total,
	}
}

// TranslateGeminiToOpenAI Gemini 非流式响应 → OpenAI chat.completion
func TranslateGeminiToOpenAI(resp *GeminiResponse, model string) map[string]any {
	now := time.Now().Unix()
	candidate := candidate0(*resp)
	if candidate == nil {
		return map[string]any{
			"id":      "chatcmpl-" + newID("chatcmpl"),
			"object":  "chat.completion",
			"created": now,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": ""},
			}},
			"usage": ParseUsage(resp.UsageMetadata),
		}
	}

	var content, reasoning string
	var toolCalls []any
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			switch {
			case part.Thought != nil && *part.Thought && part.Text != nil:
				reasoning += *part.Text
			case part.Text != nil:
				content += *part.Text
			case part.InlineData != nil:
				content += "![Generated Image](data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data + ")"
			case part.FunctionCall != nil:
				args := "{}"
				if part.FunctionCall.Args != nil {
					if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
						args = string(b)
					}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":       "call_" + newID("call"),
					"type":     "function",
					"index":    len(toolCalls),
					"function": map[string]any{"name": part.FunctionCall.Name, "arguments": args},
				})
			}
		}
	}

	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	} else {
		finish = mapFinishReason(candidate.FinishReason)
	}

	return map[string]any{
		"id":      "chatcmpl-" + newID("chatcmpl"),
		"object":  "chat.completion",
		"created": now,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": finish,
			"message":       message,
		}},
		"usage": ParseUsage(resp.UsageMetadata),
	}
}

// TranslateGeminiToOpenAIStream Gemini 流式 SSE 单事件 → OpenAI chat.completion.chunk SSE 帧。
// raw 是单个 data: 事件的 JSON（由调用方从读行循环提取）；返回可含多帧的 SSE 字节串，无内容时返回 nil。
func TranslateGeminiToOpenAIStream(raw []byte, model string, st *OpenAIStreamState) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return []byte("data: [DONE]\n\n"), nil
	}

	var resp GeminiResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, nil // 非 JSON 行忽略
	}

	if st.ID == "" {
		st.ID = "chatcmpl-" + newID("chatcmpl")
		st.Created = time.Now().Unix()
	}
	if resp.UsageMetadata != nil {
		st.Usage = ParseUsage(resp.UsageMetadata)
	}

	candidate := candidate0(resp)
	if candidate == nil {
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			errText := "[ProxySystem Error] Request blocked due to safety settings. Finish Reason: " + resp.PromptFeedback.BlockReason
			return openAIChunkFrame(st.ID, st.Created, model, map[string]any{"content": errText}, "stop", nil), nil
		}
		return nil, nil
	}

	var out []byte
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			delta := map[string]any{}
			hasContent := false
			switch {
			case part.Thought != nil && *part.Thought && part.Text != nil:
				delta["reasoning_content"] = *part.Text
				hasContent = true
			case part.Text != nil:
				delta["content"] = *part.Text
				hasContent = true
			case part.InlineData != nil:
				delta["content"] = "![Generated Image](data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data + ")"
				hasContent = true
			case part.FunctionCall != nil:
				idx := st.ToolCallIndex
				st.ToolCallIndex = idx + 1
				args := "{}"
				if part.FunctionCall.Args != nil {
					if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
						args = string(b)
					}
				}
				delta["tool_calls"] = []any{map[string]any{
					"id":       "call_" + newID("call"),
					"index":    idx,
					"type":     "function",
					"function": map[string]any{"name": part.FunctionCall.Name, "arguments": args},
				}}
				st.HasFunctionCall = true
				hasContent = true
			}
			if hasContent {
				if !st.RoleSent {
					delta["role"] = "assistant"
					st.RoleSent = true
				}
				out = append(out, openAIChunkFrame(st.ID, st.Created, model, delta, nil, nil)...)
			}
		}
	}

	if candidate.FinishReason != "" {
		finish := "stop"
		if st.HasFunctionCall {
			finish = "tool_calls"
		} else {
			finish = mapFinishReason(candidate.FinishReason)
		}
		out = append(out, openAIChunkFrame(st.ID, st.Created, model, map[string]any{}, finish, st.Usage)...)
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// openAIChunkFrame 构建一个 chat.completion.chunk SSE 帧
func openAIChunkFrame(id string, created int64, model string, delta map[string]any, finish any, usage any) []byte {
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		obj["usage"] = usage
	}
	return []byte("data: " + string(marshalJSON(obj)) + "\n\n")
}
