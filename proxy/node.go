package proxy

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	mathRand "math/rand"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/config"
)

// 健康检查共享 HTTP 客户端（低频调用，复用连接池即可）
var healthCheckClient = &http.Client{Timeout: 10 * time.Second}

type NodeStatus int32

const (
	StatusActive     NodeStatus = 0 // 正常参与轮询
	StatusIpChanging NodeStatus = 1 // 正在换公网 IP 或处于 30s 验证阶段
	StatusCooling    NodeStatus = 2 // 处于 429 熔断冷却阶段
	StatusDown       NodeStatus = 3 // 连通性异常
)

func (s NodeStatus) String() string {
	switch s {
	case StatusActive:
		return "Active"
	case StatusIpChanging:
		return "IpChanging"
	case StatusCooling:
		return "Cooling"
	case StatusDown:
		return "Down"
	default:
		return "Unknown"
	}
}

type Node struct {
	Name             string
	LANURL           string
	SupportsIPChange bool
	IPChangeCommand  string
	CooldownDuration time.Duration

	// 动态状态字段
	status            int32  // atomic NodeStatus
	sessionID         string // 当前节点绑定的 SessionID
	sessionMu         sync.RWMutex
	nextSessionRotate time.Time

	mu           sync.Mutex // 保护 coolingUntil
	coolingUntil time.Time  // 冷却截止时间

	// 统计指标
	TotalRequests uint64
	Status200Count uint64
	Status4xxCount uint64
	Status5xxCount uint64
	IPChangeCount  uint64

	secret string // 局域网请求秘钥
}

func NewNode(cfg config.NodeConfig, secret string) *Node {
	n := &Node{
		Name:             cfg.Name,
		LANURL:           cfg.LANURL,
		SupportsIPChange: cfg.SupportsIPChange,
		IPChangeCommand:  cfg.IPChangeCommand,
		CooldownDuration: cfg.CooldownDuration,
		secret:           secret,
	}
	atomic.StoreInt32(&n.status, int32(StatusActive))
	n.RotateSession()
	n.scheduleSessionRotate()
	n.scheduleDownRecovery()
	return n
}

func (n *Node) Status() NodeStatus {
	return NodeStatus(atomic.LoadInt32(&n.status))
}

func (n *Node) GetSessionID() string {
	n.sessionMu.RLock()
	defer n.sessionMu.RUnlock()
	return n.sessionID
}

func (n *Node) RotateSession() {
	n.sessionMu.Lock()
	defer n.sessionMu.Unlock()

	bytes := make([]byte, 13)
	if _, err := cryptoRand.Read(bytes); err != nil {
		n.sessionID = fmt.Sprintf("ses_%d", time.Now().UnixNano())
	} else {
		n.sessionID = "ses_" + hex.EncodeToString(bytes)
	}

	// 随机 121 ~ 240 分钟 (约 2 ~ 4 小时)
	minutes := 121 + mathRand.Intn(120) // 121 + [0, 119] = [121, 240]
	n.nextSessionRotate = time.Now().Add(time.Duration(minutes) * time.Minute)
	log.Printf("[%s] Session 自动刷新为: %s (下一次刷新时间: %s)", n.Name, n.sessionID, n.nextSessionRotate.Format("15:04:05"))
}

func (n *Node) scheduleSessionRotate() {
	go func() {
		for {
			n.sessionMu.RLock()
			nextTime := n.nextSessionRotate
			n.sessionMu.RUnlock()

			waitDur := time.Until(nextTime)
			if waitDur > 0 {
				time.Sleep(waitDur)
			}
			n.RotateSession()
		}
	}()
}

// scheduleDownRecovery 周期性对 Down 节点做健康检查，恢复后自动转回 Active 状态
func (n *Node) scheduleDownRecovery() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if n.Status() != StatusDown {
				continue
			}
			if n.healthCheck() {
				if atomic.CompareAndSwapInt32(&n.status, int32(StatusDown), int32(StatusActive)) {
					n.RotateSession()
					log.Printf("[%s] 健康检查通过，节点从 Down 状态自动恢复为 Active", n.Name)
				}
			} else {
				log.Printf("[%s] 健康检查未通过，节点保持 Down 状态，下次重试", n.Name)
			}
		}
	}()
}

// HandleRateLimit 触发限流处理逻辑 (带 CAS 并发锁控制)
func (n *Node) HandleRateLimit() {
	// 如果是可换 IP 节点
	if n.SupportsIPChange {
		// 使用 CAS 原子的将状态从 Active 切换为 IpChanging，确保同一时间只有一个协程能触发换 IP
		if atomic.CompareAndSwapInt32(&n.status, int32(StatusActive), int32(StatusIpChanging)) {
			go n.processIPChange()
		} else {
			log.Printf("[%s] 节点已经在换 IP 或处于非 Active 状态，跳过重复触发", n.Name)
		}
		return
	}

	// 如果是不支持换 IP 的节点
	if atomic.CompareAndSwapInt32(&n.status, int32(StatusActive), int32(StatusCooling)) {
		n.mu.Lock()
		n.coolingUntil = time.Now().Add(n.CooldownDuration)
		coolUntil := n.coolingUntil
		n.mu.Unlock()
		log.Printf("[%s] 节点进入冷却状态，时长: %v，截止时间: %s", n.Name, n.CooldownDuration, coolUntil.Format("15:04:05"))

		go func() {
			time.Sleep(n.CooldownDuration)
			atomic.StoreInt32(&n.status, int32(StatusActive))
			n.RotateSession()
			log.Printf("[%s] 节点冷却完毕，自动恢复为 Active 状态", n.Name)
		}()
	}
}

func (n *Node) processIPChange() {
	log.Printf("[%s] 收到 429 限流，开始异步执行换公网 IP 脚本: %s", n.Name, n.IPChangeCommand)
	atomic.AddUint64(&n.IPChangeCount, 1)

	// 1. 执行 Shell 脚本 (60秒超时控制)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", n.IPChangeCommand)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[%s] 执行换 IP 脚本失败: %v, 输出: %s", n.Name, err, string(output))
		atomic.StoreInt32(&n.status, int32(StatusDown))
		return
	}

	log.Printf("[%s] 换 IP 脚本执行成功，输出: %s。开始等待 30 秒进行网络就绪与健康检查...", n.Name, string(output))

	// 2. 精确等待 30 秒
	time.Sleep(30 * time.Second)

	// 3. 通过局域网发起健康检查测试
	if n.healthCheck() {
		n.RotateSession()
		atomic.StoreInt32(&n.status, int32(StatusActive))
		log.Printf("[%s] 健康检查通过，新公网 IP 连通正常，节点恢复 Active 状态", n.Name)
	} else {
		log.Printf("[%s] 30s 延迟后健康检查失败，节点置为 Down 状态", n.Name)
		atomic.StoreInt32(&n.status, int32(StatusDown))
	}
}

func (n *Node) healthCheck() bool {
	req, err := http.NewRequest("GET", n.LANURL+"/", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Proxy-Secret", n.secret)

	resp, err := healthCheckClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 只要 Nginx 返回非 502/504/403 则认为通畅 (OpenCode 通常会返回 404/405 或 200)
	return resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusGatewayTimeout && resp.StatusCode != http.StatusForbidden
}

type NodeSnapshot struct {
	Name              string     `json:"name"`
	LANURL            string     `json:"lan_url"`
	Status            string     `json:"status"`
	SessionID         string     `json:"session_id"`
	NextSessionRotate string     `json:"next_session_rotate"`
	SupportsIPChange  bool       `json:"supports_ip_change"`
	CoolingUntil      *time.Time `json:"cooling_until,omitempty"`
	TotalRequests     uint64     `json:"total_requests"`
	Status200Count    uint64     `json:"status_200_count"`
	Status4xxCount    uint64     `json:"status_4xx_count"`
	Status5xxCount    uint64     `json:"status_5xx_count"`
	IPChangeCount     uint64     `json:"ip_change_count"`
}

func (n *Node) Snapshot() NodeSnapshot {
	n.sessionMu.RLock()
	sessID := n.sessionID
	nextRotate := n.nextSessionRotate.Format("15:04:05")
	n.sessionMu.RUnlock()

	snap := NodeSnapshot{
		Name:              n.Name,
		LANURL:            n.LANURL,
		Status:            n.Status().String(),
		SessionID:         sessID,
		NextSessionRotate: nextRotate,
		SupportsIPChange:  n.SupportsIPChange,
		TotalRequests:     atomic.LoadUint64(&n.TotalRequests),
		Status200Count:    atomic.LoadUint64(&n.Status200Count),
		Status4xxCount:    atomic.LoadUint64(&n.Status4xxCount),
		Status5xxCount:    atomic.LoadUint64(&n.Status5xxCount),
		IPChangeCount:     atomic.LoadUint64(&n.IPChangeCount),
	}

	if n.Status() == StatusCooling {
		n.mu.Lock()
		t := n.coolingUntil
		n.mu.Unlock()
		snap.CoolingUntil = &t
	}

	return snap
}
