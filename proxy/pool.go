package proxy

import (
	"errors"
	"log"
	"sync/atomic"

	"opencode2api/config"
)

type Pool struct {
	nodes []*Node
	index uint64
}

func NewPool(cfg *config.Config) *Pool {
	p := &Pool{
		nodes: make([]*Node, 0, len(cfg.Nodes)),
	}

	for _, nodeCfg := range cfg.Nodes {
		node := NewNode(nodeCfg, cfg.Server.Secret)
		p.nodes = append(p.nodes, node)
	}

	return p
}

var ErrNoAvailableNode = errors.New("所有 VPS 代理节点均处于 429 冷却或换 IP 状态，暂无可用节点")

// GetNextNode 顺序轮询获取当前处于 Active 状态的节点
// 节点池在启动后固定不变，无需加锁；index 使用原子递增保证并发下均匀轮询
func (p *Pool) GetNextNode() (*Node, error) {
	total := len(p.nodes)
	if total == 0 {
		return nil, errors.New("节点池配置为空")
	}

	// 最多尝试轮询一整圈
	for i := 0; i < total; i++ {
		idx := atomic.AddUint64(&p.index, 1) % uint64(total)
		node := p.nodes[idx]
		if node.Status() == StatusActive {
			atomic.AddUint64(&node.TotalRequests, 1)
			return node, nil
		}
	}

	return nil, ErrNoAvailableNode
}

// Snapshots 获取所有节点的当前状态快照用于 API 统计展示
func (p *Pool) Snapshots() []NodeSnapshot {
	snaps := make([]NodeSnapshot, 0, len(p.nodes))
	for _, n := range p.nodes {
		snaps = append(snaps, n.Snapshot())
	}
	return snaps
}

func (p *Pool) Report200(node *Node) {
	atomic.AddUint64(&node.Status200Count, 1)
}

func (p *Pool) Report4xx(node *Node) {
	atomic.AddUint64(&node.Status4xxCount, 1)
}

func (p *Pool) Report5xx(node *Node) {
	atomic.AddUint64(&node.Status5xxCount, 1)
}

func (p *Pool) ReportRateLimit(node *Node) {
	atomic.AddUint64(&node.Status4xxCount, 1)
	log.Printf("[%s] 节点触发 RateLimit/FreeUsageLimitError，开始进行容错与状态转换", node.Name)
	node.HandleRateLimit()
}
