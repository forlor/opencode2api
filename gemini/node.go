package gemini

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/config"
)

// NodeStatus Gemini 节点的运行时状态
type NodeStatus int32

const (
	NodeActive  NodeStatus = iota // 0 正常参与轮询
	NodeCooling                   // 1 该节点所有 key 429 冷却（短于 key 冷却，先于 key 恢复）
	NodeDown                      // 2 网络层不可达（连接失败达阈值 → 健康检查自愈）
)

func (s NodeStatus) String() string {
	switch s {
	case NodeActive:
		return "Active"
	case NodeCooling:
		return "Cooling"
	case NodeDown:
		return "Down"
	default:
		return "Unknown"
	}
}

// probeWorker = 启动探测的有界并发数
const probeWorker = 8

// GeminiNode 一个节点 = 一个 VPS（出口 IP）+ 一组 Gemini API Key
type GeminiNode struct {
	Name      string
	LANURL    string
	MountPath string

	keyPool    *KeyPool
	secret     string // X-Proxy-Secret（探测/运行时请求都带）
	httpClient *http.Client

	status atomic.Int32
	mu     sync.Mutex
	coolingUntil time.Time

	consecConnFail atomic.Int32

	TotalRequests atomic.Uint64
	Status429     atomic.Uint64
	Status5xx     atomic.Uint64
	BannedKeys    atomic.Uint64
	Probes        atomic.Uint64
}

// newGeminiNode 构造节点并启动后台流程（冷却/宕机恢复、启动探测、Banned 慢速复探）
func newGeminiNode(cfg config.GeminiNodeConfig, gcfg *config.GeminiConfig, kp *KeyPool, client *http.Client, secret string) *GeminiNode {
	n := &GeminiNode{
		Name:       cfg.Name,
		LANURL:     cfg.LANURL,
		MountPath:  gcfg.MountPath,
		keyPool:    kp,
		secret:     secret,
		httpClient: client,
	}
	n.status.Store(int32(NodeActive))
	if len(kp.keys) > 0 {
		go n.startProbe()
	}
	go n.recoveryLoop()
	go n.reProbeBanned()
	return n
}

func (n *GeminiNode) Status() NodeStatus {
	s := NodeStatus(n.status.Load())
	if s == NodeCooling {
		n.mu.Lock()
		until := n.coolingUntil
		n.mu.Unlock()
		if until.IsZero() || time.Now().After(until) {
			if n.status.CompareAndSwap(int32(NodeCooling), int32(NodeActive)) {
				log.Printf("[%s] 节点冷却完毕，恢复 Active", n.Name)
			}
			return NodeActive
		}
		return NodeCooling
	}
	return s
}

// BaseURL 组装带挂载前缀的节点地址（handler 用它拼 Gemini REST 路径）
func (n *GeminiNode) BaseURL() string { return n.LANURL + n.MountPath }

// Client 返回节点出站 HTTP 客户端（含可选代理；长流不设整体超时）
func (n *GeminiNode) Client() *http.Client { return n.httpClient }

// KeyCount 该节点绑定的 key 总数（handler 用它作为节点内换 key 上限）
func (n *GeminiNode) KeyCount() int { return n.keyPool.Len() }

// ==================== key 获取 / 归还 ====================

func (n *GeminiNode) GetNextKey() (*GeminiKey, error) {
	k, err := n.keyPool.GetNextKey()
	if err != nil {
		return nil, err
	}
	n.TotalRequests.Add(1)
	return k, nil
}

func (n *GeminiNode) ReleaseKey(k *GeminiKey) {
	n.keyPool.ReleaseKey(k)
}

// ==================== 错误上报（含节点级判定） ====================

func (n *GeminiNode) Report200(k *GeminiKey) {
	n.keyPool.Report200(k)
}

// Report429 冷却 key；若该节点所有 key 全部不可用 → 节点短暂冷却，避免雪崩式换 key
func (n *GeminiNode) Report429(k *GeminiKey) {
	n.Status429.Add(1)
	n.keyPool.Report429(k)
	if n.keyPool.ReadyCount() == 0 {
		n.enterCooling()
	}
}

func (n *GeminiNode) Report5xx(k *GeminiKey) {
	n.Status5xx.Add(1)
	n.keyPool.Report5xx(k)
}

func (n *GeminiNode) ReportConnectionFailure(k *GeminiKey) {
	n.Status5xx.Add(1)
	n.keyPool.ReportConnectionFailure(k)
	n.consecConnFail.Add(1)
	if n.consecConnFail.Load() >= 3 {
		if n.status.CompareAndSwap(int32(NodeActive), int32(NodeDown)) {
			log.Printf("[%s] 连续连接失败达到 3 次，节点置为 Down，等待健康检查恢复", n.Name)
		}
	}
}

func (n *GeminiNode) ReportInvalidKey(k *GeminiKey, reason string) {
	n.keyPool.ReportInvalidKey(k, reason)
	n.BannedKeys.Add(1)
}

// enterCooling 节点级冷却：时长短于 key 冷却，保证先于 key 恢复、不被轮询空转
func (n *GeminiNode) enterCooling() {
	if !n.status.CompareAndSwap(int32(NodeActive), int32(NodeCooling)) {
		return
	}
	kc := n.keyPool.keyCooldown
	if kc <= 0 {
		kc = 60 * time.Second
	}
	d := kc / 2
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	d = cooldownWithJitter(d)
	n.mu.Lock()
	n.coolingUntil = time.Now().Add(d)
	n.mu.Unlock()
	log.Printf("[%s] 该节点所有 key 均被限流，节点冷却 %v", n.Name, d)
}

// recoveryLoop 周期检查：节点冷却到期恢复、Down 节点健康检查自愈
func (n *GeminiNode) recoveryLoop() {
	for {
		time.Sleep(15 * time.Second)
		switch NodeStatus(n.status.Load()) {
		case NodeCooling:
			if n.Status() == NodeActive {
				n.consecConnFail.Store(0)
				log.Printf("[%s] 节点冷却结束，恢复 Active", n.Name)
			}
		case NodeDown:
			if n.healthCheck() {
				if n.status.CompareAndSwap(int32(NodeDown), int32(NodeActive)) {
					n.consecConnFail.Store(0)
					log.Printf("[%s] 健康检查通过，节点从 Down 恢复为 Active", n.Name)
				}
			}
		}
	}
}

// healthCheck 轻量探测节点连通性（nginx 活着即可，200/4xx 均视为健康）
func (n *GeminiNode) healthCheck() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Proxy-Secret", n.secret)
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusGatewayTimeout
}

// ==================== 启动探测 / Banned 慢速复探 ====================

// startProbe 启动时以有界并发对所有 key 发轻量探测（GET /v1beta/models），
// 无效/区域封禁 key 直接置 Banned，不污染运行时轮询。探测不记入业务统计。
func (n *GeminiNode) startProbe() {
	keys := n.keyPool.keys
	jobs := make(chan *GeminiKey)
	var wg sync.WaitGroup
	for i := 0; i < probeWorker; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				kinds := n.probeKey(k)
				n.applyProbeResult(k, kinds)
			}
		}()
	}
	for _, k := range keys {
		jobs <- k
	}
	close(jobs)
	wg.Wait()
}

// reProbeBanned 每 24h 对 Banned key 慢速复探一次，临时封禁的 key 可自愈恢复
func (n *GeminiNode) reProbeBanned() {
	for {
		time.Sleep(bannedReProbeInterval)
		for _, k := range n.keyPool.keys {
			if KeyStatus(k.statusIdx.Load()) != KeyStatusBanned {
				continue
			}
			if kind := n.probeKey(k); kind == ErrNone || kind == ErrRateLimit {
				k.Unban()
				log.Printf("[%s] Banned key %s 慢速复探通过，恢复 Active", n.Name, k.ID())
			}
		}
	}
}

const bannedReProbeInterval = 24 * time.Hour

// probeKey 对单个 key 发起探测请求并分类，返回错误类型
func (n *GeminiNode) probeKey(k *GeminiKey) ErrKind {
	n.Probes.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/v1beta/models", nil)
	if err != nil {
		return ErrNone
	}
	req.Header.Set("x-goog-api-key", k.Value())
	req.Header.Set("X-Proxy-Secret", n.secret)
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return ErrNone // 网络问题不判定，留给运行时报告
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return ClassifyGeminiHttp(resp.StatusCode, body)
}

// applyProbeResult 按探测分类置状态：无效/区域封禁 → Ban；启动即限流 → 冷却
func (n *GeminiNode) applyProbeResult(k *GeminiKey, kind ErrKind) {
	switch kind {
	case ErrInvalidKey:
		if k.statusIdx.CompareAndSwap(int32(KeyStatusActive), int32(KeyStatusBanned)) {
			k.mu.Lock()
			k.bannedReason = "启动探测: API key 无效/未授权"
			k.mu.Unlock()
			n.BannedKeys.Add(1)
			log.Printf("[%s] key %s 探测无效，置为 Banned", n.Name, k.ID())
		}
	case ErrRateLimit:
		log.Printf("[%s] key %s 探测被限流，冷却后重试", n.Name, k.ID())
		k.Cool(n.keyPool.keyCooldown)
	case ErrDeterministic:
		// 4xx（如 PROJECT_BLOCKED/PROHIBITED）视为不可用，Banned，等待 24h 复探自愈
		if k.statusIdx.CompareAndSwap(int32(KeyStatusActive), int32(KeyStatusBanned)) {
			k.mu.Lock()
			k.bannedReason = "启动探测: 4xx 上游拒绝"
			k.mu.Unlock()
			n.BannedKeys.Add(1)
			log.Printf("[%s] key %s 探测返回 4xx，置为 Banned", n.Name, k.ID())
		}
	}
}

// ==================== 快照 ====================

type NodeSnapshot struct {
	Name           string        `json:"name"`
	LANURL         string        `json:"lan_url"`
	MountPath      string        `json:"mount_path"`
	Status         string        `json:"status"`
	TotalRequests  uint64        `json:"total_requests"`
	Status429      uint64        `json:"status_429"`
	Status5xx      uint64        `json:"status_5xx"`
	BannedKeys     uint64        `json:"banned_keys"`
	KeyCount       int           `json:"key_count"`
	AvailableKeys  int           `json:"available_keys"`
	CoolingUntil   *time.Time    `json:"cooling_until,omitempty"`
	Keys           []KeySnapshot `json:"keys,omitempty"`
}

func (n *GeminiNode) Snapshot(includeKeys bool) NodeSnapshot {
	s := NodeSnapshot{
		Name:          n.Name,
		LANURL:        n.LANURL,
		MountPath:     n.MountPath,
		Status:        n.Status().String(),
		TotalRequests: n.TotalRequests.Load(),
		Status429:     n.Status429.Load(),
		Status5xx:     n.Status5xx.Load(),
		BannedKeys:    n.BannedKeys.Load(),
		KeyCount:      n.keyPool.Len(),
		AvailableKeys: n.keyPool.ReadyCount(),
	}
	if NodeStatus(n.status.Load()) == NodeCooling {
		n.mu.Lock()
		t := n.coolingUntil
		n.mu.Unlock()
		s.CoolingUntil = &t
	}
	if includeKeys {
		s.Keys = n.keyPool.Snapshots()
	}
	return s
}