package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ==================== Gemini 原生 /v1beta/ 入口 ====================

var nativeReqBody = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

// 原生非流式：body 原样透传（仅注入 maxOutputTokens/safetySettings），响应字节级透传
func TestE2E_NativeGenerateContent(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-ns-key-0001"}))

	w := doPost(t, rt, "/v1beta/models/gemini-2.5-flash:generateContent", string(nativeReqBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	// 响应字节级透传
	if !bytes.Equal(w.Body.Bytes(), geminiNonStreamOK) {
		t.Fatalf("响应应字节级透传: %s", w.Body.String())
	}

	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	call := posts[0]
	if call.path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("upstream path = %q", call.path)
	}
	// 请求体：contents 保留 + 兜底注入 maxOutputTokens/safetySettings
	var sent map[string]any
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["contents"]; !ok {
		t.Fatalf("contents 应保留: %s", call.body)
	}
	gc := sent["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(8192) {
		t.Fatalf("maxOutputTokens = %v", gc["maxOutputTokens"])
	}
	ss := sent["safetySettings"].([]any)
	if len(ss) != 4 {
		t.Fatalf("safetySettings = %v", ss)
	}
}

// 原生流式：SSE 字节级透传 + query(alt=sse) 透传
func TestE2E_NativeStream(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-str-key-0001"}))

	w := doPost(t, rt, "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", string(nativeReqBody))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	// SSE 字节级透传
	if !bytes.Equal(w.Body.Bytes(), geminiStreamOK) {
		t.Fatalf("流响应应字节级透传: %q", w.Body.String())
	}
	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	if posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("upstream path = %q", posts[0].path)
	}
	if posts[0].query != "alt=sse" {
		t.Fatalf("query 应透传: %q", posts[0].query)
	}
}

// 模型后缀：-search 剥离出干净路径 + 注入 googleSearch 工具；thinkingLevel 后缀注入
func TestE2E_NativeSuffixInjection(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-sfx-key-0001"}))

	w := doPost(t, rt, "/v1beta/models/gemini-2.5-flash-search-high:generateContent", string(nativeReqBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	posts := fake.postCalls()
	if len(posts) != 1 || posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("upstream path 异常: %+v", posts)
	}
	var sent map[string]any
	if err := json.Unmarshal(posts[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	tools := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["googleSearch"]; !ok {
		t.Fatalf("应注入 googleSearch: %v", tools)
	}
	tc := sent["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "HIGH" {
		t.Fatalf("thinkingLevel = %v", tc)
	}
}

// countTokens 透传
func TestE2E_NativeCountTokens(t *testing.T) {
	ctResp := []byte(`{"totalTokens":7}`)
	fake := newFakeGeminiNode(t, fakeResp{200, ctResp})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-ct-key-0001"}))

	w := doPost(t, rt, "/v1beta/models/gemini-2.5-flash:countTokens", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), ctResp) {
		t.Fatalf("countTokens 响应应透传: %s", w.Body.String())
	}
	posts := fake.postCalls()
	if len(posts) != 1 || posts[0].path != "/gemini/v1beta/models/gemini-2.5-flash:countTokens" {
		t.Fatalf("upstream path 异常: %+v", posts)
	}
}

// 认证：x-goog-api-key 头（Gemini SDK 习惯）
func TestE2E_NativeAuthXGoogKey(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-auth-key-0001"}))

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(string(nativeReqBody)))
	req.Header.Set("x-goog-api-key", "sk-test")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("x-goog-api-key 认证失败: %d %s", w.Code, w.Body.String())
	}

	// 错误 key → 401
	req2 := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(string(nativeReqBody)))
	req2.Header.Set("x-goog-api-key", "wrong-key")
	w2 := httptest.NewRecorder()
	rt.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("错误 key 应 401: %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("错误格式应为 Gemini error: %s", w2.Body.String())
	}
}

// 认证：?key= query（校验后剥除，不透传给节点）
func TestE2E_NativeQueryKeyStripped(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-qk-key-0001"}))

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=sk-test", strings.NewReader(string(nativeReqBody)))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("query key 认证失败: %d %s", w.Code, w.Body.String())
	}
	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	if posts[0].query != "alt=sse" {
		t.Fatalf("query 中 key 应剥除: %q", posts[0].query)
	}
}

// GET /v1beta/models：本地模型列表（不打上游）
func TestE2E_NativeModelsList(t *testing.T) {
	fake := newFakeGeminiNode(t)
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-ml-key-0001"}))

	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// fallback 模型必在列表（通配 gemini-* 不展开）
	found := false
	for _, m := range resp.Models {
		if m.Name == "models/gemini-2.5-flash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("列表应含 fallback 模型: %s", w.Body.String())
	}
	// 不打上游（探测 GET 除外，无生成调用）
	if got := len(fake.postCalls()); got != 0 {
		t.Fatalf("模型列表不应打上游生成端点: %d", got)
	}
}

// GET /v1beta/models/{model}：可用模型信息 / 未知模型 404
func TestE2E_NativeModelInfo(t *testing.T) {
	fake := newFakeGeminiNode(t)
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-mi-key-0001"}))

	get := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1beta/models/"+model, nil)
		req.Header.Set("Authorization", "Bearer sk-test")
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, req)
		return w
	}

	w := get("gemini-2.5-flash")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"models/gemini-2.5-flash"`) {
		t.Fatalf("模型信息异常: %s", w.Body.String())
	}

	// 带后缀的模型也应命中（剥离后判断）
	w2 := get("gemini-2.5-flash-search")
	if w2.Code != http.StatusOK {
		t.Fatalf("后缀模型应命中: %d %s", w2.Code, w2.Body.String())
	}

	// 非本线路模型（gpt-3.5 映射到 deepseek）→ 404
	w3 := get("gpt-3.5")
	if w3.Code != http.StatusNotFound {
		t.Fatalf("非本线路模型应 404: %d", w3.Code)
	}
}

// 路径净化：模型名含 / 或 .. → 400；未知 action → 404
func TestE2E_NativePathSanitization(t *testing.T) {
	fake := newFakeGeminiNode(t)
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-native-ps-key-0001"}))

	// Go http test server 会清理 ../ 路径，用空格模型名触发净化失败
	w := doPost(t, rt, "/v1beta/models/bad%20model:generateContent", string(nativeReqBody))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法模型名应 400: %d %s", w.Code, w.Body.String())
	}
	if got := len(fake.postCalls()); got != 0 {
		t.Fatalf("非法模型不应打上游: %d", got)
	}

	w2 := doPost(t, rt, "/v1beta/models/gemini-2.5-flash:someUnknownAction", string(nativeReqBody))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("未知 action 应 404: %d", w2.Code)
	}
}

// 429 换 key：原生线路同样享受 geminiExec 重试编排
func TestE2E_Native429SwapKey(t *testing.T) {
	fake := newFakeGeminiNode(t,
		fakeResp{429, geminiErrBody("RESOURCE_EXHAUSTED", "Quota exceeded")},
		fakeResp{200, geminiNonStreamOK},
	)
	keys := []string{"AIzaSy-e2e-native-429-aaaa", "AIzaSy-e2e-native-ok-bbbb"}
	cfg := geminiTestConfig(fake.URL(), keys)
	cfg.Gemini.KeyCooldownDuration = 2 * time.Second
	rt := newTestRouter(cfg)

	w := doPost(t, rt, "/v1beta/models/gemini-2.5-flash:generateContent", string(nativeReqBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := len(fake.postCalls()); got != 2 {
		t.Fatalf("应 429 后换 key 重试成功，调用次数 = %d", got)
	}
}