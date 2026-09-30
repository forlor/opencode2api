package gemini

import (
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"opencode2api/config"
)

// Pool 所有 Gemini 节点的组合（每条节点 = 出口 IP + 一组 key）
type Pool struct {
	nodes      []*GeminiNode
	index      uint64
	gcfg       *config.GeminiConfig
	secret     string
	usageStore *dailyUsageStore // 每日用量持久化（任一节点配了 daily_limit 才启用）
}

var ErrNoAvailableGeminiNode = errors.New("Gemini 线路暂无可用节点")

// NewPool 由配置构建 Gemini 节点池。gcfg 为 nil 时返回空池（路由层保证不会走到）。
// 每个节点：绑定自己的 KeyPool（数十个 key），并启动探测/自愈后台流程；
// 任一节点配置了 daily_limit 时启用每日用量持久化（启动恢复 + 周期落盘）。
func NewPool(gcfg *config.GeminiConfig, secret string) *Pool {
	p := &Pool{gcfg: gcfg, secret: secret}
	if gcfg == nil {
		return p
	}
	// 每日配额重置时区（Google 免费额度按 PT 午夜重置；内嵌 tzdata 保证任意平台可加载）
	loc, err := time.LoadLocation(gcfg.DailyResetTZ)
	if err != nil {
		log.Printf("[gemini] daily_reset_tz %q 加载失败，回退 UTC: %v", gcfg.DailyResetTZ, err)
		loc = time.UTC
	}
	anyDailyLimit := false
	for _, nc := range gcfg.Nodes {
		if len(nc.APIKeys) == 0 {
			log.Printf("[gemini] 节点 %s 未配置 api_keys，跳过", nc.Name)
			continue
		}
		keys := make([]*GeminiKey, 0, len(nc.APIKeys))
		for _, raw := range nc.APIKeys {
			if raw != "" {
				keys = append(keys, newGeminiKey(raw))
			}
		}
		// 节点级配置覆盖全局
		cooldown := nc.KeyCooldownDuration
		if cooldown <= 0 {
			cooldown = gcfg.KeyCooldownDuration
		}
		maxRPM := nc.MaxRPM
		if maxRPM <= 0 {
			maxRPM = gcfg.MaxRPM
		}
		minInterval := nc.MinKeyInterval
		if minInterval <= 0 {
			minInterval = gcfg.MinKeyInterval
		}
		// KeyStrategy/DailyLimit 的节点级继承已在 config.parseGemini 完成，这里兜底直接构造场景
		strategy := nc.KeyStrategy
		if strategy == "" {
			strategy = gcfg.KeyStrategy
		}
		dailyLimit := nc.DailyLimit
		if dailyLimit <= 0 {
			dailyLimit = gcfg.DailyLimit
		}
		kp := newKeyPool(keyPoolOpts{
			Keys:         keys,
			KeyCooldown:  cooldown,
			BanAfterFail: gcfg.BanAfterFailures,
			MaxRPM:       maxRPM,
			MinInterval:  minInterval,
			Strategy:     strategy,
			DailyLimit:   dailyLimit,
			ResetLoc:     loc,
		})
		if dailyLimit > 0 {
			anyDailyLimit = true
		}

		proxy := nc.HTTPProxy
		if proxy == "" {
			proxy = gcfg.HTTPProxy
		}
		client := buildHTTPClient(proxy)

		node := newGeminiNode(nc, gcfg, kp, client, secret)
		p.nodes = append(p.nodes, node)
		log.Printf("[gemini] 节点 %s 上线：%d 个 key（strategy=%s, cooldown=%v, maxRPM=%d, dailyLimit=%d）",
			nc.Name, len(keys), kp.strategy, cooldown, maxRPM, kp.dailyLimit)
	}
	if len(p.nodes) > 0 && anyDailyLimit {
		usageFile := gcfg.DailyUsageFile
		if usageFile == "" {
			usageFile = "gemini_daily_usage.json" // 防 GeminiConfig 直接构造时漏默认值
		}
		p.usageStore = newDailyUsageStore(usageFile, loc)
		p.usageStore.attach(p.nodes)
		p.usageStore.restore()
		p.usageStore.start()
		log.Printf("[gemini] 每日配额已启用：用量持久化 %s，重置时区 %s", usageFile, loc)
	}
	return p
}

// FlushDailyUsage 同步落盘每日用量（优雅关闭时调用；未启用持久化时为空操作）
func (p *Pool) FlushDailyUsage() {
	if p.usageStore != nil {
		p.usageStore.writeIfChanged()
	}
}

// buildHTTPClient 构建出站 HTTP 客户端（可选全局/节点代理；长流不设整体超时）
func buildHTTPClient(proxy string) *http.Client {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		} else {
			log.Printf("[gemini] http_proxy 解析失败 %q: %v", proxy, err)
		}
	}
	return &http.Client{Transport: tr}
}

// GetNextNode 顺序轮询返回一个 Active 节点（节点池固定，index 原子递增保证均匀轮询）
func (p *Pool) GetNextNode() (*GeminiNode, error) {
	total := len(p.nodes)
	if total == 0 {
		return nil, errors.New("Gemini 线路未配置节点")
	}
	for i := 0; i < total; i++ {
		idx := int(atomic.AddUint64(&p.index, 1)) % total
		n := p.nodes[idx]
		if n.Status() == NodeActive {
			return n, nil
		}
	}
	return nil, ErrNoAvailableGeminiNode
}

func (p *Pool) Nodes() []*GeminiNode { return p.nodes }
func (p *Pool) Len() int             { return len(p.nodes) }

// Snapshots 全节点状态快照（admin 端点使用）
func (p *Pool) Snapshots(includeKeys bool) []NodeSnapshot {
	snaps := make([]NodeSnapshot, 0, len(p.nodes))
	for _, n := range p.nodes {
		snaps = append(snaps, n.Snapshot(includeKeys))
	}
	return snaps
}