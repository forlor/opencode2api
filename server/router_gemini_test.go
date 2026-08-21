package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode2api/config"
	"opencode2api/gemini"
	"opencode2api/proxy"
)

// ==================== 伪装节点 nginx 的辅助 ====================

type fakeResp struct {
	status int
	body   []byte
}

type fakeCall struct {
	method, path, key string
	query             string
	body              []byte
}

// fakeGeminiNode 伪装 VPS nginx 的 /gemini/ location → 返回预置 Gemini JSON/SSE。
// nexts 按调用顺序消耗（超出后复用最后一个）。
type fakeGeminiNode struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	calls []fakeCall
	nexts []fakeResp
}

func newFakeGeminiNode(t *testing.T, nexts ...fakeResp) *fakeGeminiNode {
	t.Helper()
	f := &fakeGeminiNode{t: t, nexts: nexts}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGeminiNode) URL() string { return f.srv.URL }

func (f *fakeGeminiNode) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Header.Get("x-goog-api-key")

	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{method: r.Method, path: r.URL.Path, key: key, query: r.URL.RawQuery, body: body})
	resp := f.respFor(r.URL.Path)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}

// respFor 在锁内调用：探测走 200，generateContent/countTokens 走配置序列
func (f *fakeGeminiNode) respFor(path string) fakeResp {
	lp := strings.ToLower(path)
	if !strings.Contains(lp, "generatecontent") && !strings.Contains(lp, "counttokens") {
		return fakeResp{200, []byte(`{"models":[{"name":"models/gemini-2.5-flash"}]}`)}
	}
	if len(f.nexts) == 0 {
		return fakeResp{200, []byte(`{"candidates":[]}`)}
	}
	if len(f.nexts) > 1 {
		r := f.nexts[0]
		f.nexts = f.nexts[1:]
		return r
	}
	return f.nexts[0]
}

// postCalls 只返回真正的生成请求（排除启动探测 GET /v1beta/models）
func (f *fakeGeminiNode) postCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, 0, len(f.calls))
	for _, c := range f.calls {
		lp := strings.ToLower(c.path)
		if c.method == http.MethodPost && (strings.Contains(lp, "generatecontent") || strings.Contains(lp, "counttokens")) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGeminiNode) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// maskKeyID 复刻 gemini.maskKeyID 的脱敏规则，供测试断言
func maskKeyID(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// ==================== 响应样例 ====================

var geminiNonStreamOK = []byte(`{"candidates":[{"content":{"parts":[{"text":"Hello from Gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"thoughtsTokenCount":2,"totalTokenCount":14}}`)

var geminiStreamOK = []byte(
	"data: {\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"thinking...\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello \"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"world\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":6,\"thoughtsTokenCount\":2,\"totalTokenCount\":13}}\n\n")

func geminiErrBody(status, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": 400, "status": status, "message": message},
	})
	return b
}

// ==================== 测试辅助 ====================

func geminiTestConfig(nodeURL string, keys []string) *config.Config {
	cfg := &config.Config{}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.APIKeys = []string{"sk-test"}
	cfg.Server.Secret = "lan-secret"
	cfg.Default.FallbackModel = "deepseek-v4-flash-free"
	cfg.Default.ModelMappings = map[string]string{
		"gpt-4o":  "gemini-2.5-flash",
		"gpt-3.5": "deepseek-v4-flash-free",
	}
	cfg.Gemini = &config.GeminiConfig{
		Enabled:                 true,
		FallbackModel:           "gemini-2.5-flash",
		DefaultMaxOutputTokens:  8192,
		Models:                  []string{"gemini-*"},
		MountPath:               "/gemini",
		StreamingMode:           "real",
		SafetySettingsThreshold: "OFF",
		KeyCooldownDuration:     50 * time.Millisecond,
		BanAfterFailures:        3,
		Nodes: []config.GeminiNodeConfig{{
			Name:    "gem-fake",
			LANURL:  nodeURL,
			APIKeys: keys,
		}},
	}
	return cfg
}

func newTestRouter(cfg *config.Config) *Router {
	return NewRouter(cfg, proxy.NewPool(cfg), gemini.NewPool(cfg.Gemini, cfg.Server.Secret))
}

func doPost(t *testing.T, rt *Router, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	return w
}

// ==================== 用例 ====================

// OpenAI 非流式：模型映射命中 Gemini → 转换后转发 → OpenAI chat.completion 返回
func TestE2E_GeminiNonStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-nonstream-key-0001"}))

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello from Gemini" {
		t.Fatalf("content = %v", msg["content"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["completion_tokens"] != float64(9) {
		t.Fatalf("usage = %v", usage)
	}

	// 上游请求：路径已净化、key 走 header、secret 透传、body 为 Gemini 格式
	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	call := posts[0]
	if call.path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("upstream path = %q", call.path)
	}
	if call.key != "AIzaSy-e2e-nonstream-key-0001" {
		t.Fatalf("x-goog-api-key = %q", call.key)
	}
	var gr map[string]any
	if err := json.Unmarshal(call.body, &gr); err != nil {
		t.Fatal(err)
	}
	if _, ok := gr["contents"]; !ok {
		t.Fatalf("upstream body 应为 Gemini contents: %s", call.body)
	}
}

// OpenAI 流式：SSE 转换（内含 thinking→reasoning_content、正文、finish、usage、[DONE]）
func TestE2E_GeminiStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-stream-key-0001"}))

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"reasoning_content":"thinking..."`) {
		t.Fatalf("缺少 thinking 帧: %s", body)
	}
	if !strings.Contains(body, `"content":"Hello "`) || !strings.Contains(body, `"content":"world"`) {
		t.Fatalf("缺少正文帧: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("缺少 finish 帧: %s", body)
	}
	if !strings.Contains(body, `"reasoning_tokens":2`) {
		t.Fatalf("缺少 usage 明细: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("应以 [DONE] 结束: %s", body)
	}
}

// 429 后换 key 重试：首个 key 429 冷却，换 key 后成功
func TestE2E_Gemini429RetrySwapKey(t *testing.T) {
	fake := newFakeGeminiNode(t,
		fakeResp{429, geminiErrBody("RESOURCE_EXHAUSTED", "Quota exceeded")},
		fakeResp{200, geminiNonStreamOK},
	)
	keys := []string{"AIzaSy-e2e-limit-key-aaaa", "AIzaSy-e2e-good-key-bbbb"}
	cfg := geminiTestConfig(fake.URL(), keys)
	cfg.Gemini.KeyCooldownDuration = 2 * time.Second // 冷却时长 > 测试窗口，保证断言时仍为 Cooling
	rt := newTestRouter(cfg)

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	// 两次上游生成调用：一次 429 + 一次成功
	calls := fake.postCalls()
	if len(calls) != 2 {
		t.Fatalf("上游生成调用次数 = %d, want 2", len(calls))
	}
	// 第一个被使用的 key 应已被冷却（快照只出脱敏 ID）
	wantCooled := maskKeyID(calls[0].key)
	n := rt.gemPool.Nodes()[0]
	var cooled, active int
	for _, ks := range n.Snapshot(true).Keys {
		if ks.ID == wantCooled && ks.Status == "Cooling" {
			cooled++
		}
		if ks.Status == "Active" {
			active++
		}
	}
	if cooled != 1 || active != 1 {
		t.Fatalf("应恰好 1 个 key Cooling、1 个 Active，快照=%+v", n.Snapshot(true).Keys)
	}
}

// 确定性 400：透传错误体，不烧 key（仅一次调用）
func TestE2E_GeminiDeterministicError(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{400, geminiErrBody("INVALID_ARGUMENT", "Bad request body")})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-det-key-0001"}))

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("应透传 Gemini 错误体: %s", w.Body.String())
	}
	if got := len(fake.postCalls()); got != 1 {
		t.Fatalf("确定性错误不应重试换 key, 生成调用次数 = %d", got)
	}
	n := rt.gemPool.Nodes()[0]
	for _, ks := range n.Snapshot(true).Keys {
		if ks.Status != "Active" {
			t.Fatalf("确定性错误不应冷却/禁用 key: %+v", ks)
		}
	}
}

// 回归：未配置 Gemini 时恒走 OpenCode 节点线路
func TestE2E_OpenCodeRegression(t *testing.T) {
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"echo":` + string(body) + `}`))
	}))
	defer oc.Close()

	cfg := &config.Config{}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.APIKeys = []string{"sk-test"}
	cfg.Server.Secret = "lan-secret"
	cfg.Default.FallbackModel = "deepseek-v4-flash-free"
	cfg.Default.ModelMappings = map[string]string{"gpt-4o": "deepseek-v4-flash-free"}
	cfg.Nodes = []config.NodeConfig{{Name: "oc-fake", LANURL: oc.URL}}
	rt := NewRouter(cfg, proxy.NewPool(cfg), nil) // gemPool 为 nil

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("应透传 OpenCode 节点响应: %s", w.Body.String())
	}
}

// OpenAI 流式 + fake 模式：非流式缓冲后以 SSE 分帧发给客户端
func TestE2E_GeminiFakeStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	cfg := geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-fake-key-0001"})
	cfg.Gemini.StreamingMode = "fake"
	rt := newTestRouter(cfg)

	w := doPost(t, rt, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"Hello from Gemini"`) || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("fake 流异常: %s", body)
	}
	// fake 模式应走非流式端点（generateContent 不带 stream）
	if posts := fake.postCalls(); len(posts) != 1 || posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("fake 模式请求异常: %+v", posts)
	}
}

// /admin/gemini 快照：key 脱敏、不泄露完整 key
func TestE2E_AdminGemini(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-admin-secret-key-0001"}))

	req := httptest.NewRequest(http.MethodGet, "/admin/gemini", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "AIzaSy-admin-secret-key-0001") {
		t.Fatalf("admin 快照泄露完整 key: %s", body)
	}
	if !strings.Contains(body, "AIzaSy...0001") {
		t.Fatalf("应包含脱敏 key ID: %s", body)
	}
}