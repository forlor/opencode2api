package gemini

import (
	"encoding/json"
	"testing"
	"time"
)

func TestClassifyErrorStatus_DailyVsRateLimit(t *testing.T) {
	cases := []struct {
		status, message string
		want            ErrKind
	}{
		// 每日配额（RPD）型：PerDay / per day / daily quota 特征
		{"RESOURCE_EXHAUSTED", "You exceeded your quota limit GenerateRequestsPerDayPerProjectPerModel-free-tier Limit 20 Exceeded", ErrDailyQuota},
		{"RESOURCE_EXHAUSTED", "Daily quota exceeded, will reset at midnight", ErrDailyQuota},
		{"RATE_LIMIT_EXCEEDED", "resource exhausted: per day quota hit", ErrDailyQuota},
		// 分钟级 RPM/TPM 型
		{"RESOURCE_EXHAUSTED", "RPM request limit exceeded. Please retry after 27s", ErrRateLimit},
		{"RESOURCE_EXHAUSTED", "GenerateRequestsPerMinutePerProjectPerModel-free-tier Limit Exceeded", ErrRateLimit},
		{"RESOURCE_EXHAUSTED", "GenerateTokensPerMinutePerProjectModel limit exceeded", ErrRateLimit},
		// 无 daily 特征的未知 429 → 保守按分钟级短冷却
		{"RESOURCE_EXHAUSTED", "Quota exceeded for quota metric 'Generate requests'", ErrRateLimit},
		{"RATE_LIMIT_EXCEEDED", "", ErrRateLimit},
	}
	for _, c := range cases {
		if got := ClassifyErrorStatus(c.status, c.message); got != c.want {
			t.Errorf("ClassifyErrorStatus(%q, %q) = %v, want %v", c.status, c.message, got, c.want)
		}
	}
}

// ClassifyGeminiHttp 组合分类：HTTP 429 + daily 消息体 → ErrDailyQuota
func TestClassifyGeminiHttp_DailyQuota(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    429,
			"status":  "RESOURCE_EXHAUSTED",
			"message": "You exceeded your quota limit ... GenerateRequestsPerDayPerProjectPerModel-free-tier ... Limit: 20 ... Exceeded",
		},
	})
	if got := ClassifyGeminiHttp(429, body); got != ErrDailyQuota {
		t.Fatalf("429 daily 消息应分类为 ErrDailyQuota，得到 %v", got)
	}
	// 裸 429 无结构化体 → 保守 ErrRateLimit
	if got := ClassifyGeminiHttp(429, []byte("too many requests")); got != ErrRateLimit {
		t.Fatalf("裸 429 应分类为 ErrRateLimit，得到 %v", got)
	}
}

func TestParseRetryDelay(t *testing.T) {
	withRetryInfo := `{"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"27s"}]}}`
	if d, ok := ParseRetryDelay([]byte(withRetryInfo)); !ok || d != 27*time.Second {
		t.Fatalf("应解析出 27s，得到 %v %v", d, ok)
	}

	// 分钟级小数
	frac := `{"error":{"code":429,"message":"x","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"90.5s"}]}}`
	if d, ok := ParseRetryDelay([]byte(frac)); !ok || d != 90500*time.Millisecond {
		t.Fatalf("应解析出 90.5s，得到 %v %v", d, ok)
	}

	// 无 RetryInfo
	noRetry := `{"error":{"code":429,"message":"x","status":"RESOURCE_EXHAUSTED"}}`
	if _, ok := ParseRetryDelay([]byte(noRetry)); ok {
		t.Fatal("无 RetryInfo 应返回 false")
	}

	// 畸形 retryDelay
	malformed := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"soon"}]}}`
	if _, ok := ParseRetryDelay([]byte(malformed)); ok {
		t.Fatal("畸形 retryDelay 应返回 false")
	}

	// 非 JSON
	if _, ok := ParseRetryDelay([]byte("not json")); ok {
		t.Fatal("非 JSON 应返回 false")
	}
}
