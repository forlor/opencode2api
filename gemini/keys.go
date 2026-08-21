package gemini

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

var ErrAllKeysUnavailable = errors.New("该节点所有 key 均不可用（冷却/禁用/被占用）")

// KeyStatus GeminiKey 的运行时状态
type KeyStatus int32

const (
	KeyStatusActive  KeyStatus = iota // 正常参与轮询
	KeyStatusCooling                  // 429 冷却中（到时自动恢复 Active）
	KeyStatusBanned                   // 无效 key / 连续失败，永久禁用（24h 慢速复探可自愈）
)

func (s KeyStatus) String() string {
	switch s {
	case KeyStatusActive:
		return "Active"
	case KeyStatusCooling:
		return "Cooling"
	case KeyStatusBanned:
		return "Banned"
	default:
		return "Unknown"
	}
}

// maskKeyID 脱敏 key：AIzaSy...后4位（admin 快照/日志用，绝不输出完整 key）
func maskKeyID(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// GeminiKey 单个 AIza... key 的状态机（并发安全，均在 atomic/CAS 上完成）
type GeminiKey struct {
	key string // 完整 key，仅本节点内存持有
	id  string // 脱敏 ID

	statusIdx      atomic.Int32
	inFlight       atomic.Int32 // 占用标记：命中后 +1、请求结束（流关/超时）-1
	consecFailures atomic.Int32 // 连续 5xx/连接失败，达到阈值 → Ban
	TotalRequests  atomic.Uint64
	Status200      atomic.Uint64
	Status4xx      atomic.Uint64
	Status5xx      atomic.Uint64
	Status429      atomic.Uint64

	mu            sync.Mutex // 保护以下低频字段
	coolingUntil  int64      // unixNano，0 表示未冷却
	bannedReason  string
	lastError     string
	lastErrorAt   int64 // unixNano
	lastUsedAt    int64 // unixNano
	rpmMinute     int64 // 当前 RPM 计数的分钟（now.Unix()/60）
	rpmWindowUsed int
}

func newGeminiKey(key string) *GeminiKey {
	k := &GeminiKey{key: key, id: maskKeyID(key)}
	k.statusIdx.Store(int32(KeyStatusActive))
	return k
}

func (k *GeminiKey) ID() string    { return k.id }
func (k *GeminiKey) Value() string { return k.key }
func (k *GeminiKey) InFlight() int { return int(k.inFlight.Load()) }

// Status 返回当前状态；Cooling 到期时顺带自愈回 Active
func (k *GeminiKey) Status() KeyStatus { return k.effectiveStatus(time.Now()) }

func (k *GeminiKey) effectiveStatus(now time.Time) KeyStatus {
	s := KeyStatus(k.statusIdx.Load())
	if s != KeyStatusCooling {
		return s
	}
	k.mu.Lock()
	until := k.coolingUntil
	k.mu.Unlock()
	if until == 0 || now.UnixNano() >= until {
		// 冷却到期自动恢复（CAS 防覆盖 Banned）
		if k.statusIdx.CompareAndSwap(int32(KeyStatusCooling), int32(KeyStatusActive)) {
			k.consecFailures.Store(0)
		}
		return KeyStatusActive
	}
	return KeyStatusCooling
}

// cooldownWithJitter 给冷却时长加 ±~50% 随机抖动，避免一批 key 同时到期引发"冷却风暴"
func cooldownWithJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(base/2)))
}

// Cool 进入冷却（带随机抖动）。Banned 的 key 不受影响。
func (k *GeminiKey) Cool(base time.Duration) {
	d := cooldownWithJitter(base)
	k.CoolUntil(d)
}

// CoolUntil 进入冷却，冷却起点为当前时间
func (k *GeminiKey) CoolUntil(d time.Duration) {
	if k.statusIdx.Load() == int32(KeyStatusBanned) {
		return
	}
	k.statusIdx.Store(int32(KeyStatusCooling))
	k.mu.Lock()
	k.coolingUntil = time.Now().Add(d).UnixNano()
	k.mu.Unlock()
}

// Ban 禁用 key（启动探测无效/403/连续失败），reProbe 可自愈
func (k *GeminiKey) Ban(reason string) {
	k.statusIdx.Store(int32(KeyStatusBanned))
	k.mu.Lock()
	k.bannedReason = reason
	k.mu.Unlock()
}

// Unban 慢速复探通过后恢复 Active
func (k *GeminiKey) Unban() {
	k.mu.Lock()
	k.bannedReason = ""
	k.lastError = ""
	k.mu.Unlock()
	k.consecFailures.Store(0)
	k.statusIdx.Store(int32(KeyStatusActive))
}

func (k *GeminiKey) recordError(errMsg string) {
	k.mu.Lock()
	k.lastError = errMsg
	k.lastErrorAt = time.Now().UnixNano()
	k.mu.Unlock()
}

func (k *GeminiKey) bannedReasonText() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bannedReason
}

// rpmExceeded 检查当前分钟内的用量是否已达 maxRPM 上限
func (k *GeminiKey) rpmExceeded(now time.Time, maxRPM int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	minute := now.Unix() / 60
	if k.rpmMinute != minute {
		k.rpmMinute = minute
		k.rpmWindowUsed = 0
	}
	return k.rpmWindowUsed >= maxRPM
}

// tryAcquire 原子占用 key：inFlight CAS 0→1，避免并发下同一 key 被同时选中。
// 占用成功时更新统计、最近使用时间与 RPM 窗口。
func (k *GeminiKey) tryAcquire(now time.Time) bool {
	if !k.inFlight.CompareAndSwap(0, 1) {
		return false
	}
	k.TotalRequests.Add(1)
	k.mu.Lock()
	k.lastUsedAt = now.UnixNano()
	minute := now.Unix() / 60
	if k.rpmMinute != minute {
		k.rpmMinute = minute
		k.rpmWindowUsed = 0
	}
	k.rpmWindowUsed++
	k.mu.Unlock()
	return true
}

// release 请求结束（成功/失败/超时）归还占用标记
func (k *GeminiKey) release() {
	k.inFlight.Add(-1)
}

// KeyPool 一个节点绑定的整组 key 的轮询与报告入口
type KeyPool struct {
	keys  []*GeminiKey
	index uint64

	keyCooldown    time.Duration
	banAfterFail   int32
	maxRPM         int
	minInterval    time.Duration
}

func newKeyPool(keys []*GeminiKey, keyCooldown time.Duration, banAfterFail int32, maxRPM int, minInterval time.Duration) *KeyPool {
	return &KeyPool{
		keys:         keys,
		keyCooldown:  keyCooldown,
		banAfterFail: banAfterFail,
		maxRPM:       maxRPM,
		minInterval:  minInterval,
	}
}

func (kp *KeyPool) Len() int { return len(kp.keys) }

// GetNextKey 原子轮询一圈，命中一个可用 key 并占用（inFlight+1）。
// 调用方必须在请求结束（含长流关闭/超时）时 ReleaseKey 归还，否则长流期间该 key 不会被并发选中。
func (kp *KeyPool) GetNextKey() (*GeminiKey, error) {
	total := len(kp.keys)
	if total == 0 {
		return nil, errors.New("节点无可用 key（api_keys 为空）")
	}
	now := time.Now()
	for i := 0; i < total; i++ {
		idx := int(atomic.AddUint64(&kp.index, 1)) % total
		k := kp.keys[idx]
		if !kp.keyAvailable(k, now) {
			continue
		}
		if k.tryAcquire(now) {
			return k, nil
		}
		// CAS 抢占失败（并发下另一协程刚占用）→ 换下一个 key
	}
	return nil, ErrAllKeysUnavailable
}

func (kp *KeyPool) keyAvailable(k *GeminiKey, now time.Time) bool {
	if k.effectiveStatus(now) != KeyStatusActive {
		return false
	}
	if k.inFlight.Load() > 0 {
		return false
	}
	if kp.maxRPM > 0 && k.rpmExceeded(now, kp.maxRPM) {
		return false
	}
	if kp.minInterval > 0 {
		k.mu.Lock()
		lu := k.lastUsedAt
		k.mu.Unlock()
		if lu > 0 && now.Sub(time.Unix(0, lu)) < kp.minInterval {
			return false
		}
	}
	return true
}

// ReleaseKey 归还占用标记
func (kp *KeyPool) ReleaseKey(k *GeminiKey) {
	k.release()
}

// ReadyCount 当前"可用（状态 Active，不考虑 in-flight 与 rpm 瞬时限制）"的 key 数
func (kp *KeyPool) ReadyCount() int {
	now := time.Now()
	n := 0
	for _, k := range kp.keys {
		if k.effectiveStatus(now) == KeyStatusActive {
			n++
		}
	}
	return n
}

// Report200 请求成功：清零连续失败计数
func (kp *KeyPool) Report200(k *GeminiKey) {
	k.Status200.Add(1)
	k.consecFailures.Store(0)
}

// Report429 限流：冷却该 key
func (kp *KeyPool) Report429(k *GeminiKey) {
	k.Status429.Add(1)
	k.recordError("429 rate limited")
	k.Cool(kp.keyCooldown)
}

// Report5xx 上游 5xx：连续失败计数，达到阈值 Ban 该 key
func (kp *KeyPool) Report5xx(k *GeminiKey) {
	k.Status5xx.Add(1)
	k.recordError("5xx server error")
	if k.bumpFailures(kp.banAfterFail) {
		k.Ban("连续 5xx 失败 (ban_after_failures)")
	}
}

// ReportConnectionFailure 网络连接失败：连续计数，达到阈值 Ban
func (kp *KeyPool) ReportConnectionFailure(k *GeminiKey) {
	k.Status5xx.Add(1)
	k.recordError("connection failed")
	if k.bumpFailures(kp.banAfterFail) {
		k.Ban("连接失败 (ban_after_failures)")
	}
}

// ReportInvalidKey 403 / API key not valid：直接 Ban
func (kp *KeyPool) ReportInvalidKey(k *GeminiKey, reason string) {
	k.Status4xx.Add(1)
	k.recordError(reason)
	k.Ban(reason)
}

// bumpFailures 递增连续失败计数，返回是否超过 ban 阈值
func (k *GeminiKey) bumpFailures(limit int32) bool {
	return int(k.consecFailures.Add(1)) >= int(limit)
}

// KeySnapshot admin 快照中的单 key 状态（绝不输出完整 key）
type KeySnapshot struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	InFlight      int32  `json:"in_flight"`
	TotalRequests uint64 `json:"total_requests"`
	Status200     uint64 `json:"status_200"`
	Status4xx     uint64 `json:"status_4xx"`
	Status5xx     uint64 `json:"status_5xx"`
	Status429     uint64 `json:"status_429"`
	BannedReason  string `json:"banned_reason,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	CoolingUntil  string `json:"cooling_until,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
}

func (kp *KeyPool) Snapshots() []KeySnapshot {
	out := make([]KeySnapshot, 0, len(kp.keys))
	now := time.Now()
	for _, k := range kp.keys {
		k.mu.Lock()
		breason := k.bannedReason
		lerr := k.lastError
		coolingUntil := k.coolingUntil
		lastUsed := k.lastUsedAt
		k.mu.Unlock()
		s := KeySnapshot{
			ID:            k.id,
			Status:        k.effectiveStatus(now).String(),
			InFlight:      k.inFlight.Load(),
			TotalRequests: k.TotalRequests.Load(),
			Status200:     k.Status200.Load(),
			Status4xx:     k.Status4xx.Load(),
			Status5xx:     k.Status5xx.Load(),
			Status429:     k.Status429.Load(),
			BannedReason:  breason,
			LastError:     lerr,
		}
		if s.Status == "Cooling" && coolingUntil > 0 {
			s.CoolingUntil = time.Unix(0, coolingUntil).Format("15:04:05")
		}
		if lastUsed > 0 {
			s.LastUsedAt = time.Unix(0, lastUsed).Format("15:04:05")
		}
		out = append(out, s)
	}
	return out
}