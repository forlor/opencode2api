package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"opencode2api/config"
	"opencode2api/gemini"
)

// handleGeminiNative Gemini 原生 /v1beta/ 入口：
//
//	POST /v1beta/models/{model}:generateContent[?alt=sse]
//	POST /v1beta/models/{model}:streamGenerateContent[?alt=sse]
//	POST /v1beta/models/{model}:countTokens
//	GET  /v1beta/models[/{model}]
//
// 请求体原样透传（仅注入后缀/线级开关），复用 geminiExec 的节点/key 轮换与错误分类。
// 认证兼容三种 Gemini 客户端习惯：Authorization: Bearer、x-goog-api-key 头、?key= query
//（query 中的 key 校验后剥除，不透传给节点 nginx，避免落入 access_log）。
func (r *Router) handleGeminiNative(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 线路未启用时一律 404（正常情况下路由未注册，此处防御性兜底）
	if r.cfg.Gemini == nil || !r.cfg.Gemini.Enabled {
		nativeNotFound(w, req.URL.Path)
		return
	}

	if !r.nativeAuthOK(req) {
		writeRawJSON(w, http.StatusUnauthorized,
			[]byte(`{"error":{"code":401,"message":"Invalid API key","status":"UNAUTHENTICATED"}}`))
		return
	}

	path := req.URL.Path
	rest, ok := strings.CutPrefix(path, "/v1beta/")
	if !ok || rest == "" {
		nativeNotFound(w, path)
		return
	}

	// GET /v1beta/models：本地模型列表（不打上游）
	if rest == "models" || rest == "models/" {
		if req.Method != http.MethodGet {
			nativeMethodNotAllowed(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(r.geminiModelsCache)
		return
	}

	if model, isModels := strings.CutPrefix(rest, "models/"); isModels {
		// POST models/{model}:{action}
		if idx := strings.LastIndex(model, ":"); idx > 0 {
			action := model[idx+1:]
			model = model[:idx]
			if req.Method != http.MethodPost {
				nativeMethodNotAllowed(w)
				return
			}
			switch action {
			case "generateContent", "streamGenerateContent", "countTokens":
			default:
				writeRawJSON(w, http.StatusNotFound, mustMarshal(map[string]any{
					"error": map[string]any{
						"code": 404, "status": "NOT_FOUND",
						"message": "Unknown Gemini action: :" + action,
					},
				}))
				return
			}
			r.handleGeminiNativeAction(w, req, model, action)
			return
		}
		// GET /v1beta/models/{model}：单模型信息
		if req.Method != http.MethodGet {
			nativeMethodNotAllowed(w)
			return
		}
		r.nativeModelInfo(w, model)
		return
	}

	nativeNotFound(w, path)
}

// handleGeminiNativeAction POST models/{model}:{action}：模型后缀剥离 + 请求体微调 + geminiExec 编排
func (r *Router) handleGeminiNativeAction(w http.ResponseWriter, req *http.Request, model, action string) {
	limitMB := inboundBodyLimitMB(r.cfg)
	req.Body = http.MaxBytesReader(w, req.Body, int64(limitMB)<<20) // 与 OpenAI 线路一致，可配
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Printf("[req-stats] path=%s 读请求体失败(疑似超过%dMB入站上限): %v (已读 %.2fMB)",
			req.URL.Path, limitMB, err, float64(len(body))/mbSize)
		writeRawJSON(w, http.StatusBadRequest,
			[]byte(`{"error":{"code":400,"message":"Failed to read request body","status":"INVALID_ARGUMENT"}}`))
		return
	}

	// 原生协议入站即 Gemini 格式，复用统一统计（解析失败不影响转发）
	var nativePayload map[string]any
	if json.Unmarshal(body, &nativePayload) == nil && nativePayload != nil {
		logRequestStats(req.URL.Path, nativePayload, len(body), limitMB)
	}

	suffix := gemini.ParseModelSuffixes(model)
	modelPath := sanitizeGeminiModel(suffix.CleanModel)
	if modelPath == "" {
		writeRawJSON(w, http.StatusBadRequest, mustMarshal(map[string]any{
			"error": map[string]any{
				"code": 400, "status": "INVALID_ARGUMENT",
				"message": "Invalid model name: " + model,
			},
		}))
		return
	}

	gcfg := r.cfg.Gemini
	body = gemini.InjectNativeTweaks(body, suffix, gemini.ConvertOpts{
		ForceWebSearch:          gcfg.ForceWebSearch,
		ForceCodeExecution:      gcfg.ForceCodeExecution,
		ForceUrlContext:         gcfg.ForceUrlContext,
		SafetyThreshold:         gcfg.SafetySettingsThreshold,
		MaxOutputTokensFallback: gcfg.DefaultMaxOutputTokens,
	})

	// query 透传（剥掉 ?key=），如 streamGenerateContent 的 alt=sse
	q := req.URL.Query()
	q.Del("key")
	endpoint := ":" + action
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	isStream := action == "streamGenerateContent"
	r.geminiExec(w, req, body, modelPath, endpoint, func(w http.ResponseWriter, req *http.Request, node *gemini.GeminiNode, key *gemini.GeminiKey, resp *http.Response) {
		if isStream {
			// 原生协议入站=上游：字节级透传（流首预检已在 geminiExec 内完成）
			forwardRawGeminiStream(w, resp)
			node.ReleaseKey(key)
			return
		}
		respBytes, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		node.ReleaseKey(key)
		if rerr != nil {
			writeRawJSON(w, http.StatusBadGateway,
				[]byte(`{"error":{"code":502,"message":"Failed to read upstream response","status":"UNAVAILABLE"}}`))
			return
		}
		writeRawJSON(w, http.StatusOK, respBytes)
	})
}

// nativeAuthOK /v1beta/ 入口认证：Bearer > x-goog-api-key > ?key=（均对 api_keys 常量比较）
func (r *Router) nativeAuthOK(req *http.Request) bool {
	keys := r.cfg.Server.APIKeys
	if len(keys) == 0 {
		return true // 未配置 API key 时放行（与现有 auth 一致）
	}
	match := func(token string) bool {
		for _, k := range keys {
			if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
				return true
			}
		}
		return false
	}
	if h := req.Header.Get("Authorization"); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return match(strings.TrimSpace(parts[1]))
		}
	}
	if k := req.Header.Get("x-goog-api-key"); k != "" {
		return match(k)
	}
	if k := req.URL.Query().Get("key"); k != "" {
		return match(k)
	}
	return false
}

// nativeModelInfo GET /v1beta/models/{model}：后缀剥离后按分流规则判断是否本线路可用
func (r *Router) nativeModelInfo(w http.ResponseWriter, model string) {
	clean := gemini.ParseModelSuffixes(model).CleanModel
	if r.cfg.RouteRequest(clean).Line != config.LineGemini {
		writeRawJSON(w, http.StatusNotFound, mustMarshal(map[string]any{
			"error": map[string]any{
				"code": 404, "status": "NOT_FOUND",
				"message": "Model " + model + " is not supported by this gateway",
			},
		}))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":                      "models/" + clean,
		"displayName":               clean,
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"},
	})
}

// buildGeminiModelsCache 预构建 GET /v1beta/models 响应：
// ClientModels + Models 非通配项 + FallbackModel（与 RouteRequest 接受的模型集合一致）
func (r *Router) buildGeminiModelsCache() {
	gcfg := r.cfg.Gemini
	set := map[string]bool{}
	for _, m := range gcfg.ClientModels {
		if m != "" {
			set[m] = true
		}
	}
	for _, m := range gcfg.Models {
		if m != "" && !strings.ContainsAny(m, "*?[") {
			set[m] = true
		}
	}
	if gcfg.FallbackModel != "" {
		set[gcfg.FallbackModel] = true
	}

	names := make([]string, 0, len(set))
	for m := range set {
		names = append(names, m)
	}
	sort.Strings(names)

	models := make([]map[string]any, 0, len(names))
	for _, m := range names {
		models = append(models, map[string]any{
			"name":                      "models/" + m,
			"displayName":               m,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"},
		})
	}
	r.geminiModelsCache = mustMarshal(map[string]any{"models": models})
}

// forwardRawGeminiStream 原生流式字节级透传（无需格式转换，按块读 + 逐块 flush）
func forwardRawGeminiStream(w http.ResponseWriter, resp *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	idleTimer := time.AfterFunc(streamIdleTimeout, func() { resp.Body.Close() })
	defer idleTimer.Stop()

	buf := copyBufPool.Get().([]byte)
	defer copyBufPool.Put(buf)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			idleTimer.Reset(streamIdleTimeout)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

func nativeNotFound(w http.ResponseWriter, path string) {
	writeRawJSON(w, http.StatusNotFound, mustMarshal(map[string]any{
		"error": map[string]any{
			"code": 404, "status": "NOT_FOUND",
			"message": "Unknown Gemini endpoint: " + path,
		},
	}))
}

func nativeMethodNotAllowed(w http.ResponseWriter) {
	writeRawJSON(w, http.StatusMethodNotAllowed, mustMarshal(map[string]any{
		"error": map[string]any{
			"code": 405, "status": "METHOD_NOT_ALLOWED",
			"message": "Method not allowed",
		},
	}))
}