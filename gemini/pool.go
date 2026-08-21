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
	nodes  []*GeminiNode
	index  uint64
	gcfg   *config.GeminiConfig
	secret string
}

var ErrNoAvailableGeminiNode = errors.New("Gemini 线路暂无可用节点")

// NewPool 由配置构建 Gemini 节点池。gcfg 为 nil 时返回空池（路由层保证不会走到）。
// 每个节点：绑定自己的 KeyPool（数十个 key），并启动探测/自愈后台流程。
func NewPool(gcfg *config.GeminiConfig, secret string) *Pool {
	p := &Pool{gcfg: gcfg, secret: secret}
	if gcfg == nil {
		return p
	}
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
		kp := newKeyPool(keys, cooldown, gcfg.BanAfterFailures, maxRPM, minInterval)

		proxy := nc.HTTPProxy
		if proxy == "" {
			proxy = gcfg.HTTPProxy
		}
		client := buildHTTPClient(proxy)

		node := newGeminiNode(nc, gcfg, kp, client, secret)
		p.nodes = append(p.nodes, node)
		log.Printf("[gemini] 节点 %s 上线：%d 个 key（cooldown=%v, maxRPM=%d）", nc.Name, len(keys), cooldown, maxRPM)
	}
	return p
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