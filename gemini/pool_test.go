package gemini

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opencode2api/config"
)

func geminiErrBody(status, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": 400, "status": status, "message": message},
	})
	return b
}

func waitCond(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", desc)
}

// testGeminiNode 构造不含后台 goroutine 的节点（单测直接操控状态机）
func testGeminiNode(lanURL string, keys []*GeminiKey, cooldown time.Duration, banAfter int32, client *http.Client) *GeminiNode {
	kp := newKeyPool(keys, cooldown, banAfter, 0, 0)
	n := &GeminiNode{
		Name:       "test-node",
		LANURL:     lanURL,
		MountPath:  "/gemini",
		keyPool:    kp,
		secret:     "s",
		httpClient: client,
	}
	n.status.Store(int32(NodeActive))
	return n
}

func TestNode_AllKeys429_NodeCooling(t *testing.T) {
	keys := testKeys(2, "n429")
	n := testGeminiNode("http://127.0.0.1:1", keys, 50*time.Millisecond, 3, http.DefaultClient)

	n.Report429(keys[0])
	if n.Status() != NodeActive { // 还有一个 key 可用
		t.Fatalf("单 key 429 后节点不应冷却: %v", n.Status())
	}
	n.Report429(keys[1])
	if n.Status() != NodeCooling {
		t.Fatalf("全部 key 冷却后节点应进入 Cooling: %v", n.Status())
	}

	// 节点冷却到期自动恢复 Active
	n.mu.Lock()
	n.coolingUntil = time.Now().Add(-time.Millisecond)
	n.mu.Unlock()
	if n.Status() != NodeActive {
		t.Fatalf("节点冷却到期应恢复 Active: %v", n.Status())
	}
	// key 按抖动冷却时长（≤75ms）逐个恢复，轮询等待全部回到可用
	waitCond(t, time.Second, "全部 key 恢复", func() bool { return n.keyPool.ReadyCount() == 2 })
	if n.keyPool.ReadyCount() != 2 {
		t.Fatalf("key 应全部恢复并再次可用, ReadyCount=%d", n.keyPool.ReadyCount())
	}
}

func TestNode_ConnectionFailureDown(t *testing.T) {
	keys := testKeys(2, "down")
	n := testGeminiNode("http://127.0.0.1:1", keys, time.Millisecond, 10, http.DefaultClient)

	for i := 0; i < 3; i++ {
		n.ReportConnectionFailure(keys[0])
	}
	if n.Status() != NodeDown {
		t.Fatalf("连续 3 次连接失败节点应 Down: %v", n.Status())
	}
	// banAfterFail=10 足够高，key 不应被误 Ban
	if keys[0].Status() != KeyStatusActive {
		t.Fatalf("连接失败不应误 Ban key: %v", keys[0].Status())
	}
}

func TestNode_InvalidKeyBanReports(t *testing.T) {
	keys := testKeys(1, "inv")
	n := testGeminiNode("http://127.0.0.1:1", keys, time.Millisecond, 3, http.DefaultClient)

	n.ReportInvalidKey(keys[0], "403 API key not valid")
	if keys[0].Status() != KeyStatusBanned {
		t.Fatalf("403 应 Ban key: %v", keys[0].Status())
	}
	if n.BannedKeys.Load() != 1 {
		t.Fatalf("BannedKeys 计数 = %d", n.BannedKeys.Load())
	}
}

func TestPool_GetNextNodeAndAllCooling(t *testing.T) {
	a := testGeminiNode("http://127.0.0.1:1", testKeys(1, "a"), time.Millisecond, 3, http.DefaultClient)
	b := testGeminiNode("http://127.0.0.1:1", testKeys(1, "b"), time.Millisecond, 3, http.DefaultClient)
	p := &Pool{nodes: []*GeminiNode{a, b}}

	node, err := p.GetNextNode()
	if err != nil {
		t.Fatal(err)
	}
	if node != a && node != b {
		t.Fatalf("返回未知节点")
	}

	// 冷却当前节点 → 应返回另一个
	node.enterCooling()
	other, err := p.GetNextNode()
	if err != nil {
		t.Fatal(err)
	}
	if other == node {
		t.Fatalf("冷却中的节点不应被选中")
	}
	if other == a {
		a.enterCooling()
	} else {
		b.enterCooling()
	}
	if _, err := p.GetNextNode(); err != ErrNoAvailableGeminiNode {
		t.Fatalf("两节点均冷却应返回 ErrNoAvailableGeminiNode, got %v", err)
	}
}

// fakeNodeNginx 伪装节点 nginx：/gemini/v1beta/models 按 x-goog-api-key 返回不同结果，
// 用于验证启动探测的 Ban/冷却分类。
func fakeNodeNginx(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Proxy-Secret") != "sec" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := r.Header.Get("x-goog-api-key")
		switch {
		case len(key) >= 8 && key[:8] == "bad-key-":
			w.WriteHeader(http.StatusForbidden)
			w.Write(geminiErrBody("PERMISSION_DENIED", "API key not valid. Please pass a valid API key."))
		case len(key) >= 10 && key[:10] == "limit-key-":
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write(geminiErrBody("RESOURCE_EXHAUSTED", "Quota exceeded for quota metric 'Generate requests'."))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-flash"}]}`))
		}
	}))
	return srv
}

func TestNode_StartupProbe(t *testing.T) {
	srv := fakeNodeNginx(t)
	defer srv.Close()

	gcfg := &config.GeminiConfig{
		MountPath:           "/gemini",
		KeyCooldownDuration: 20 * time.Millisecond,
		BanAfterFailures:    3,
		Nodes: []config.GeminiNodeConfig{{
			Name:    "probe-node",
			LANURL:  srv.URL,
			APIKeys: []string{"good-key-1", "bad-key-1", "limit-key-1"},
		}},
	}
	p := NewPool(gcfg, "sec")
	if p.Len() != 1 {
		t.Fatalf("节点数 = %d", p.Len())
	}
	n := p.nodes[0]
	var good, bad, limit *GeminiKey
	for _, k := range n.keyPool.keys {
		switch {
		case k.Value() == "good-key-1":
			good = k
		case k.Value() == "bad-key-1":
			bad = k
		case k.Value() == "limit-key-1":
			limit = k
		}
	}

	waitCond(t, 3*time.Second, "探测完成", func() bool {
		return good.Status() == KeyStatusActive &&
			bad.Status() == KeyStatusBanned &&
			limit.Status() == KeyStatusCooling
	})
	if bad.bannedReasonText() == "" {
		t.Fatalf("Banned key 应记录原因")
	}
}

func TestPool_NoNodes(t *testing.T) {
	p := NewPool(&config.GeminiConfig{Nodes: nil}, "sec")
	if p.Len() != 0 {
		t.Fatalf("无节点时 Len 应 0")
	}
	if _, err := p.GetNextNode(); err == nil {
		t.Fatalf("空池 GetNextNode 应报错")
	}
}

func TestNode_Snapshot(t *testing.T) {
	keys := testKeys(2, "snap")
	n := testGeminiNode("http://127.0.0.1:1", keys, time.Millisecond, 3, http.DefaultClient)
	keys[1].Ban("403 test")
	s := n.Snapshot(true)
	if s.Status != "Active" || s.KeyCount != 2 || s.AvailableKeys != 1 {
		t.Fatalf("快照异常: %+v", s)
	}
	if len(s.Keys) != 2 || s.Keys[1].Status != "Banned" || s.Keys[1].ID == keys[1].Value() {
		t.Fatalf("key 快照应脱敏: %+v", s.Keys)
	}
}