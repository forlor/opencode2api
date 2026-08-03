package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"opencode2api/adapter"
	"opencode2api/config"
	"opencode2api/middleware"
	"opencode2api/proxy"
)

type Router struct {
	cfg  *config.Config
	pool *proxy.Pool
	mux  *http.ServeMux
}

func NewRouter(cfg *config.Config, pool *proxy.Pool) *Router {
	r := &Router{
		cfg:  cfg,
		pool: pool,
		mux:  http.NewServeMux(),
	}

	r.setupRoutes()
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) setupRoutes() {
	authMW := middleware.AuthMiddleware(r.cfg)

	// API 路由
	r.mux.Handle("/v1/chat/completions", authMW(http.HandlerFunc(r.handleChatCompletions)))
	r.mux.Handle("/v1/messages", authMW(http.HandlerFunc(r.handleChatCompletions)))
	r.mux.Handle("/v1/responses", authMW(http.HandlerFunc(r.handleChatCompletions)))
	r.mux.Handle("/v1/models", authMW(http.HandlerFunc(r.handleModels)))

	// 监控 API
	r.mux.HandleFunc("/admin/nodes", r.handleAdminNodes)
}

func (r *Router) handleChatCompletions(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var openAIReq adapter.OpenAIRequest
	if err := json.Unmarshal(bodyBytes, &openAIReq); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// 最多重试节点的总数量次数
	maxRetries := len(r.cfg.Nodes)
	if maxRetries == 0 {
		http.Error(w, `{"error": "No nodes available in pool"}`, http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: 120 * time.Second}

	// 根据客户端请求的模型，查询实际映射的目标模型
	targetModel := r.cfg.GetMappedModel(openAIReq.Model)
	log.Printf("收到请求 model: %s -> 映射为 targetModel: %s", openAIReq.Model, targetModel)

	for attempt := 0; attempt < maxRetries; attempt++ {
		// 1. 获取一个 Active 节点
		node, err := r.pool.GetNextNode()
		if err != nil {
			log.Printf("获取可用节点失败: %v", err)
			http.Error(w, `{"error": "All nodes in cooling or ip changing state"}`, http.StatusServiceUnavailable)
			return
		}

		// 2. 构建转发请求
		httpReq, err := adapter.BuildOpenCodeHTTPRequest(node, &openAIReq, targetModel, r.cfg.Server.Secret)
		if err != nil {
			log.Printf("[%s] 构建请求失败: %v", node.Name, err)
			r.pool.Report4xx(node)
			continue
		}

		// 3. 发起请求 (如果遇到 503 等服务端抖动，进行最多 3 次指数退避重试: 1s, 2s, 4s)
		var resp *http.Response
		var respErr error

		for retry := 0; retry < 3; retry++ {
			if retry > 0 {
				backoff := time.Duration(1<<(retry-1)) * time.Second
				log.Printf("[%s] 遇到服务端临时异常(%d)，进行第 %d 次指数退避重试，等待 %v...", node.Name, resp.StatusCode, retry, backoff)
				time.Sleep(backoff)
				// 重新构建请求 Header
				httpReq, _ = adapter.BuildOpenCodeHTTPRequest(node, &openAIReq, targetModel, r.cfg.Server.Secret)
			}

			resp, respErr = client.Do(httpReq)
			if respErr != nil {
				r.pool.Report5xx(node)
				continue
			}

			// 如果不是 503/502/500 服务端抖动，说明拿到确定性响应 (200 / 429)，跳出退避重试
			if !adapter.IsTemporaryServerError(resp.StatusCode) {
				break
			}
			r.pool.Report5xx(node)
			resp.Body.Close()
		}

		if respErr != nil || resp == nil {
			log.Printf("[%s] 请求局域网 Nginx 失败: %v", node.Name, respErr)
			r.pool.Report5xx(node)
			continue
		}

		// 4. 检查是否触发 429 / FreeUsageLimitError
		errBody, isRateLimit, err := adapter.ReadAndCheckStreamInitialError(resp)
		if isRateLimit || err != nil {
			resp.Body.Close()
			log.Printf("[%s] 节点响应异常或触发 429 限流，执行换 IP / 冷却，自动切换下一节点", node.Name)
			r.pool.ReportRateLimit(node)
			continue // 自动无感重试下一个节点
		}

		// 请求成功 (200 OK)
		r.pool.Report200(node)

		// 5. 区分 Stream 与非 Stream 响应
		if openAIReq.Stream {
			defer resp.Body.Close()
			if err := adapter.HandleStreamForwarding(w, resp); err != nil {
				log.Printf("[%s] SSE 推流异常: %v", node.Name, err)
			}
			return
		}

		// 非 Stream 响应直接返回
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		if len(errBody) > 0 {
			w.Write(errBody)
		} else {
			io.Copy(w, resp.Body)
		}
		return
	}

	http.Error(w, `{"error": "All retry attempts failed due to rate limits or connection errors"}`, http.StatusBadGateway)
}

func (r *Router) handleModels(w http.ResponseWriter, req *http.Request) {
	modelSet := make(map[string]bool)

	// 加入映射列表里的所有模型
	for k := range r.cfg.Default.ModelMappings {
		modelSet[k] = true
	}
	// 加入兜底模型
	if r.cfg.Default.FallbackModel != "" {
		modelSet[r.cfg.Default.FallbackModel] = true
	}

	modelList := make([]map[string]interface{}, 0, len(modelSet))
	for modelName := range modelSet {
		modelList = append(modelList, map[string]interface{}{
			"id":       modelName,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "opencode",
		})
	}

	resp := map[string]interface{}{
		"object": "list",
		"data":   modelList,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (r *Router) handleAdminNodes(w http.ResponseWriter, req *http.Request) {
	snaps := r.pool.Snapshots()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_nodes": len(snaps),
		"nodes":       snaps,
	})
}
