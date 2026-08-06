package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"opencode2api/adapter"
	"opencode2api/config"
	"opencode2api/middleware"
	"opencode2api/proxy"
)

// 共享 HTTP Transport，复用底层 TCP/TLS 连接池，避免每个请求重建连接
var sharedTransport = &http.Transport{
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   50,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 90 * time.Second,
}

// 非流式请求使用总超时，避免上游挂死拖住请求
var nonStreamClient = &http.Client{
	Transport: sharedTransport,
	Timeout:   120 * time.Second,
}

// 流式(SSE)请求不设总超时，避免长生成被截断；
// 依赖 ResponseHeaderTimeout 保证响应头及时返回，避免无响应挂死
var streamClient = &http.Client{Transport: sharedTransport}

// 非流式响应的 Copy 缓冲池（io.CopyBuffer 的 buf 不能并发复用，需每请求独立分配）
var copyBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 256*1024)
	},
}

// setCORS 为跨域请求添加 CORS 响应头
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

type Router struct {
	cfg         *config.Config
	pool        *proxy.Pool
	mux         *http.ServeMux
	modelsCache []byte // 模型列表响应缓存（配置加载后不变）
}

func NewRouter(cfg *config.Config, pool *proxy.Pool) *Router {
	r := &Router{
		cfg:  cfg,
		pool: pool,
		mux:  http.NewServeMux(),
	}

	r.buildModelsCache()
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
	setCORS(w)
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 转发到节点时保持与入口相同的路径，使 Anthropic(messages)/OpenAI(chat) 协议原样透传
	apiPath := req.URL.Path

	// 限制请求体大小，防止恶意超大 body 导致内存耗尽
	req.Body = http.MaxBytesReader(w, req.Body, 10<<20) // 10MB

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// 只解析一次为通用 map，保留客户端所有参数
	payload, err := adapter.ParseOpenAIRequest(bodyBytes)
	if err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// 提取 stream / model 字段用于分流与模型映射
	isStream, _ := payload["stream"].(bool)
	clientModel, _ := payload["model"].(string)

	// 最多重试节点的总数量次数
	maxRetries := len(r.cfg.Nodes)
	if maxRetries == 0 {
		http.Error(w, `{"error": "No nodes available in pool"}`, http.StatusInternalServerError)
		return
	}

	// 根据客户端请求的模型，查询实际映射的目标模型
	targetModel := r.cfg.GetMappedModel(clientModel)
	log.Printf("收到请求 model: %s -> 映射为 targetModel: %s", clientModel, targetModel)

	httpClient := nonStreamClient
	if isStream {
		httpClient = streamClient
	}

	for attempt := 0; attempt < maxRetries; attempt++ {
		// 1. 获取一个 Active 节点
		node, err := r.pool.GetNextNode()
		if err != nil {
			log.Printf("获取可用节点失败: %v", err)
			http.Error(w, `{"error": "All nodes in cooling or ip changing state"}`, http.StatusServiceUnavailable)
			return
		}

		// 2. 构建转发请求
		httpReq, err := adapter.BuildOpenCodeHTTPRequest(req.Context(), node, payload, apiPath, targetModel, r.cfg.Server.Secret)
		if err != nil {
			log.Printf("[%s] 构建请求失败: %v", node.Name, err)
			r.pool.Report4xx(node)
			continue
		}

		// 3. 发起请求 (如果遇到 503 等服务端抖动，进行最多 3 次指数退避重试: 1s, 2s, 4s)
		var resp *http.Response
		var respErr error
		buildFailed := false

		for retry := 0; retry < 3; retry++ {
			if retry > 0 {
				// 上一次请求可能连接失败导致 resp 为 nil，需判空再读取状态码
				lastStatus := 0
				if resp != nil {
					lastStatus = resp.StatusCode
				}
				backoff := time.Duration(1<<(retry-1)) * time.Second
				if lastStatus == 0 {
					log.Printf("[%s] 上一次请求连接失败，无 HTTP 响应，进行第 %d 次指数退避重试，等待 %v...", node.Name, retry, backoff)
				} else {
					log.Printf("[%s] 遇到服务端临时异常(%d)，进行第 %d 次指数退避重试，等待 %v...", node.Name, lastStatus, retry, backoff)
				}
				// 客户端已断开时不再空等退避，直接终止重试
				select {
				case <-time.After(backoff):
				case <-req.Context().Done():
					log.Printf("[%s] 客户端已断开，终止重试", node.Name)
					return
				}
				// 重新构建请求 Header
				httpReq, err = adapter.BuildOpenCodeHTTPRequest(req.Context(), node, payload, apiPath, targetModel, r.cfg.Server.Secret)
				if err != nil {
					log.Printf("[%s] 重试时构建请求失败: %v", node.Name, err)
					r.pool.Report4xx(node)
					buildFailed = true
					break
				}
			}

			resp, respErr = httpClient.Do(httpReq)
			if respErr != nil {
				if isTimeoutError(respErr) {
					log.Printf("[%s] 第 %d 次请求等待响应头超时 (nginx 存活、上游响应慢): %v", node.Name, retry+1, respErr)
					r.pool.ReportTimeout(node)
				} else {
					log.Printf("[%s] 第 %d 次请求局域网 Nginx 连接失败: %v (URL: %s)", node.Name, retry+1, respErr, httpReq.URL.String())
					r.pool.ReportConnectionFailure(node)
				}
				continue
			}

			// 如果不是 503/502/500 服务端抖动，说明拿到确定性响应 (200 / 429)，跳出退避重试
			if !adapter.IsTemporaryServerError(resp.StatusCode) {
				break
			}
			r.pool.Report5xx(node)
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
			resp.Body.Close()
			log.Printf("[%s] 服务端临时异常响应详情 (第 %d 次): %s", node.Name, retry+1, truncateLog(bodyBytes))
		}

		// 重试期间构建请求失败，直接切换下一节点
		if buildFailed {
			continue
		}

		if respErr != nil || resp == nil {
			if isTimeoutError(respErr) {
				log.Printf("[%s] 请求等待响应头超时(3 次重试后)，切换下一节点: %v", node.Name, respErr)
				r.pool.ReportTimeout(node)
			} else {
				log.Printf("[%s] 请求局域网 Nginx 失败(3 次重试后): %v (URL: %s)", node.Name, respErr, httpReq.URL.String())
				r.pool.ReportConnectionFailure(node)
			}
			continue
		}

		// 内部指数退避重试耗尽后仍为 5xx，视为该节点服务端抖动，切换下一节点
		if adapter.IsTemporaryServerError(resp.StatusCode) {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
			resp.Body.Close()
			log.Printf("[%s] 重试 3 次后仍为服务端临时错误 %d，自动切换下一节点，详情: %s", node.Name, resp.StatusCode, truncateLog(bodyBytes))
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

		// 非 200 响应（如 400/404/422）为确定性业务错误，透传状态码与错误体，不计为成功
		if resp.StatusCode != http.StatusOK {
			r.pool.Report4xx(node)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			if len(errBody) > 0 {
				w.Write(errBody)
			} else {
				buf := copyBufPool.Get().([]byte)
				_, cErr := io.CopyBuffer(w, resp.Body, buf)
				copyBufPool.Put(buf)
				if cErr != nil {
					log.Printf("[%s] 透传错误响应失败: %v", node.Name, cErr)
				}
			}
			resp.Body.Close()
			return
		}

		// 请求成功 (200 OK)
		r.pool.Report200(node)

		// 5. 区分 Stream 与非 Stream 响应
		if isStream {
			if err := adapter.HandleStreamForwarding(w, resp); err != nil {
				log.Printf("[%s] SSE 推流异常: %v", node.Name, err)
			}
			resp.Body.Close()
			return
		}

		// 非 Stream 响应直接返回
		w.Header().Set("Content-Type", "application/json")
		if len(errBody) > 0 {
			w.Write(errBody)
		} else {
			buf := copyBufPool.Get().([]byte)
			_, cErr := io.CopyBuffer(w, resp.Body, buf)
			copyBufPool.Put(buf)
			if cErr != nil {
				log.Printf("[%s] 透传响应失败: %v", node.Name, cErr)
			}
		}
		resp.Body.Close()
		return
	}

	http.Error(w, `{"error": "All retry attempts failed due to rate limits or connection errors"}`, http.StatusBadGateway)
}

func (r *Router) handleModels(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(r.modelsCache)
}

// buildModelsCache 根据配置预构建模型列表响应（配置加载后固定，无需每次请求重建）
func (r *Router) buildModelsCache() {
	modelSet := make(map[string]bool)

	// 加入映射列表里的所有模型
	for k := range r.cfg.Default.ModelMappings {
		modelSet[k] = true
	}
	// 加入兜底模型
	if r.cfg.Default.FallbackModel != "" {
		modelSet[r.cfg.Default.FallbackModel] = true
	}

	modelNames := make([]string, 0, len(modelSet))
	for modelName := range modelSet {
		modelNames = append(modelNames, modelName)
	}
	sort.Strings(modelNames)

	modelList := make([]map[string]interface{}, 0, len(modelNames))
	created := time.Now().Unix()
	for _, modelName := range modelNames {
		modelList = append(modelList, map[string]interface{}{
			"id":       modelName,
			"object":   "model",
			"created":  created,
			"owned_by": "opencode",
		})
	}

	resp := map[string]interface{}{
		"object": "list",
		"data":   modelList,
	}

	r.modelsCache, _ = json.Marshal(resp)
}

func (r *Router) handleAdminNodes(w http.ResponseWriter, req *http.Request) {
	snaps := r.pool.Snapshots()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_nodes": len(snaps),
		"nodes":       snaps,
	})
}

// truncateLog 截断响应体日志，避免长错误信息刷爆日志，最多保留 2000 字节
func truncateLog(b []byte) string {
	const maxLen = 2000
	if len(b) > maxLen {
		return string(b[:maxLen]) + fmt.Sprintf("... (truncated %d bytes)", len(b)-maxLen)
	}
	return string(b)
}

// isTimeoutError 判断错误是否为超时类型 (ResponseHeaderTimeout 或客户端总超时触发)
func isTimeoutError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
