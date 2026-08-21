package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func mustParsePayload(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("解析测试 payload 失败: %v", err)
	}
	return m
}

// Anthropic /v1/messages：base64 图计入体积，url 图计入 urlImgs
func TestCollectRequestStatsAnthropic(t *testing.T) {
	payload := mustParsePayload(t, `{
		"model": "claude-x", "stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hi"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]}
		]
	}`)
	st := collectRequestStats(payload)
	if st.msgs != 2 {
		t.Errorf("msgs = %d, want 2", st.msgs)
	}
	if st.imgs != 1 || st.imgBytes != 4 {
		t.Errorf("imgs = %d imgBytes = %d, want 1 / 4", st.imgs, st.imgBytes)
	}
	if st.urlImgs != 1 {
		t.Errorf("urlImgs = %d, want 1", st.urlImgs)
	}
}

// OpenAI chat completions：data URI 取逗号后 base64 长度，http URL 计 urlImgs
func TestCollectRequestStatsOpenAIChat(t *testing.T) {
	payload := mustParsePayload(t, `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "看图"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,MTIzNA=="}}
			]},
			{"role": "user", "content": [
				{"type": "image_url", "image_url": {"url": "https://example.com/b.jpg"}}
			]}
		]
	}`)
	st := collectRequestStats(payload)
	if st.msgs != 2 {
		t.Errorf("msgs = %d, want 2", st.msgs)
	}
	// "data:image/png;base64," 前缀 22 字节 + base64 数据 8 字节
	if st.imgs != 1 || st.imgBytes != 8 {
		t.Errorf("imgs = %d imgBytes = %d, want 1 / 8", st.imgs, st.imgBytes)
	}
	if st.urlImgs != 1 {
		t.Errorf("urlImgs = %d, want 1", st.urlImgs)
	}
}

// OpenAI Responses input_image：image_url 为字符串形态
func TestCollectRequestStatsResponsesInputImage(t *testing.T) {
	payload := mustParsePayload(t, `{
		"model": "gpt-5",
		"input": [
			{"type": "input_image", "image_url": "https://example.com/c.jpg"},
			{"type": "input_image", "image_url": "data:image/jpeg;base64,QUJD"}
		]
	}`)
	st := collectRequestStats(payload)
	if st.msgs != 0 {
		t.Errorf("msgs = %d, want 0（Responses 协议无 messages 字段）", st.msgs)
	}
	if st.imgs != 1 || st.imgBytes != 4 {
		t.Errorf("imgs = %d imgBytes = %d, want 1 / 4", st.imgs, st.imgBytes)
	}
	if st.urlImgs != 1 {
		t.Errorf("urlImgs = %d, want 1", st.urlImgs)
	}
}

// Gemini 原生：inlineData(camelCase) 与 inline_data(snake_case) 都识别，msgs 取 contents
func TestCollectRequestStatsGeminiNative(t *testing.T) {
	payload := mustParsePayload(t, `{
		"contents": [
			{"role": "user", "parts": [
				{"text": "看图"},
				{"inlineData": {"mimeType": "image/png", "data": "MTIz"}}
			]},
			{"role": "user", "parts": [
				{"inline_data": {"mime_type": "image/jpeg", "data": "QQ=="}}
			]}
		]
	}`)
	st := collectRequestStats(payload)
	if st.msgs != 2 {
		t.Errorf("msgs = %d, want 2", st.msgs)
	}
	if st.imgs != 2 || st.imgBytes != 8 {
		t.Errorf("imgs = %d imgBytes = %d, want 2 / 8", st.imgs, st.imgBytes)
	}
}

// 纯文本请求各项为零；tool_result 内嵌结构不误计
func TestCollectRequestStatsPlainText(t *testing.T) {
	payload := mustParsePayload(t, `{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "普通文本"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "f", "input": {"path": "a.go"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]}
		]
	}`)
	st := collectRequestStats(payload)
	if st.msgs != 3 || st.imgs != 0 || st.imgBytes != 0 || st.urlImgs != 0 {
		t.Errorf("stats = %+v, want msgs=3 其余为 0", st)
	}
}

// 空/畸形 payload 不 panic
func TestCollectRequestStatsDegenerate(t *testing.T) {
	for _, raw := range []string{`{}`, `{"messages": null}`, `{"contents": "not-array"}`, `{"messages": [null, 1, "x"]}`} {
		payload := mustParsePayload(t, raw)
		st := collectRequestStats(payload)
		if st.imgs != 0 || st.imgBytes != 0 || st.urlImgs != 0 {
			t.Errorf("payload %s: stats = %+v, want 全零图片统计", raw, st)
		}
	}
}

// 端到端：带 base64 图的 Anthropic 请求走通 Gemini Claude 线路。
// 用 go test -run TestE2E_RequestStatsWithImage -v 运行可观察
// [req-stats] 入口统计行与出站对比行的实际输出格式。
func TestE2E_RequestStatsWithImage(t *testing.T) {
	fake := newFakeGeminiNode(t, fakeResp{200, geminiNonStreamOK})
	rt := newTestRouter(geminiTestConfig(fake.URL(), []string{"AIzaSy-e2e-stats-img-key-0001"}))

	body := `{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
	]}]}`
	w := doPost(t, rt, "/v1/messages", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	// 上游收到的 Gemini 请求应含内联图片（统计对象与转发内容一致）
	posts := fake.postCalls()
	if len(posts) != 1 {
		t.Fatalf("生成调用次数 = %d", len(posts))
	}
	if !strings.Contains(string(posts[0].body), `"inlineData"`) {
		t.Fatalf("上游请求缺少 inlineData: %s", posts[0].body)
	}
}
