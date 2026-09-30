package gemini

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

var ErrAllKeysUnavailable = errors.New("该节点所有 key 均不可用（冷却/禁用/被占用/当日配额耗尽）")

// key 选择策略
const (
	strategyRoundRobin = "round_robin" // 轮询（默认，兼容旧行为）
	strategySequential = "sequential"  // 顺序消耗：主力 key 配额耗尽/不可用才顺移到下一个，指针不回退
)

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

// modelDayUsage 单模型单日用量（GeminiKey.mu 保护）
type modelDayUsage struct {
	day            string // 重置时区自然日 "2006-01-02"，跨天清零
	used           int    // 当日已用次数
	exhaustedUntil int64  // 该模型当日配额耗尽后的重置时刻 unixNano，0=未耗尽
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
	modelDay      map[string]*modelDayUsage // model → 当日用量
	sweepDay      string                    // 上次全量清扫 modelDay 的自然日（跨天首次计数时清扫一次）
}

func newGeminiKey(key string) *GeminiKey {
	k := &GeminiKey{key: key, id: maskKeyID(key), modelDay: map[string]*modelDayUsage{}}
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

// maxTrackedModels 单 key 追踪的模型用量条目上限：原生入口的模型名由客户端可控，
// 超限时不再为新模型建条目（该模型配额统计退化为不计数，请求仍正常转发），防止 map 无界增长。
const maxTrackedModels = 256

// sweepStaleModelsLocked 删除全部非今日条目（跨天残留），防止 modelDay 无界增长。调用方须持有 k.mu。
func (k *GeminiKey) sweepStaleModelsLocked(today string) {
	for m, u := range k.modelDay {
		if u.day != today {
			delete(k.modelDay, m)
		}
	}
}

// dayUsageLocked 取/建该模型的当日用量（每自然日首次计数时全量清扫跨天残留）。
// 调用方须持有 k.mu；now 必须已转换到重置时区（自然日边界按该时区计算）。
// 返回 nil 表示条目数已达上限，调用方应跳过计数（请求仍正常转发）。
func (k *GeminiKey) dayUsageLocked(now time.Time, model string) *modelDayUsage {
	today := now.Format("2006-01-02")
	if k.sweepDay != today {
		k.sweepStaleModelsLocked(today)
		k.sweepDay = today
	}
	u := k.modelDay[model]
	if u != nil {
		return u // sweep 已删除跨天条目，现存条目必属今日
	}
	if len(k.modelDay) >= maxTrackedModels {
		return nil
	}
	u = &modelDayUsage{day: today}
	k.modelDay[model] = u
	return u
}

// modelQuotaExhausted 该 key 对 model 的当日配额是否耗尽；顺带做到期自愈（惰性，无需后台协程）。
// 429 上报的耗尽标记（exhaustedUntil）不受 dailyLimit 是否启用影响——否则未配 daily_limit 的
// 默认部署下 daily-429 既不冷却也不跳过，sequential 会把整轮重试烧在同一把耗尽 key 上。
// now 必须已转换到重置时区。
func (k *GeminiKey) modelQuotaExhausted(now time.Time, model string, dailyLimit int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	u := k.modelDay[model]
	if u == nil {
		return false
	}
	if u.day != now.Format("2006-01-02") {
		return false // 跨天：下次计数时 dayUsageLocked 会清零
	}
	if u.exhaustedUntil > 0 {
		if now.UnixNano() >= u.exhaustedUntil {
			u.used = 0
			u.exhaustedUntil = 0
			return false
		}
		return true
	}
	if dailyLimit <= 0 {
		return false // 未启用每日限额：只有 429 上报的耗尽标记生效
	}
	return u.used >= dailyLimit
}

// markModelExhausted 标记该 key 对 model 的当日配额耗尽（到次日重置时刻）。
// per-model 语义：key 对模型 A 耗尽不影响其对模型 B 可用，因此不用 key 级 Cooling 状态表示。
// 条目已属于昨天时直接丢弃该 429——慢响应跨过重置午夜（或上游重置传播延迟）时，
// 新一天计数尚未产生，不能把全新的一天封锁到 D+2 午夜（标记会持久化，重启也无法解除）。
func (k *GeminiKey) markModelExhausted(model string, loc *time.Location) {
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	k.mu.Lock()
	defer k.mu.Unlock()
	u := k.modelDay[model]
	if u == nil {
		u = &modelDayUsage{day: today}
		k.modelDay[model] = u
	} else if u.day != today {
		return // 跨天到达的旧配额日 429：不封锁新的一天
	}
	u.exhaustedUntil = nextDailyReset(now, loc).UnixNano()
}

// restoreDailyUsage 启动时从持久化文件恢复单模型当日用量（day 必须已校验等于今天）
func (k *GeminiKey) restoreDailyUsage(day, model string, used int, exhaustedUntilRFC string) {
	var until int64
	if exhaustedUntilRFC != "" {
		if t, err := time.Parse(time.RFC3339Nano, exhaustedUntilRFC); err == nil {
			until = t.UnixNano()
		}
	}
	k.mu.Lock()
	k.modelDay[model] = &modelDayUsage{day: day, used: used, exhaustedUntil: until}
	k.mu.Unlock()
}

// nextDailyReset loc 时区次日 00:00（Google 免费额度按太平洋时间午夜重置）
func nextDailyReset(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	y, m, d := n.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}

// tryAcquire 原子占用 key：inFlight CAS 0→1，避免并发下同一 key 被同时选中。
// 占用成功时更新统计、最近使用时间、RPM 窗口与 model 当日用量（达到 dailyLimit 即标记耗尽，
// 不影响当前在途请求，下次选择自然跳过）。now 必须已转换到重置时区。
// chargeDaily=false（countTokens 类端点）不消耗当日配额——Google RPD 只计生成请求，
// 否则 countTokens 密集的客户端会把有效生成配额减半。
func (k *GeminiKey) tryAcquire(now time.Time, model string, dailyLimit int, chargeDaily bool) bool {
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
	if chargeDaily {
		if u := k.dayUsageLocked(now, model); u != nil {
			u.used++
			if dailyLimit > 0 && u.used >= dailyLimit {
				u.exhaustedUntil = nextDailyReset(now, now.Location()).UnixNano()
			}
		}
	}
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

	keyCooldown  time.Duration
	banAfterFail int32
	maxRPM       int
	minInterval  time.Duration

	strategy   string        // round_robin | sequential
	dailyLimit int           // 每 key 每模型每日上限，0=不限制
	resetLoc   *time.Location // 每日配额重置时区

	sticky   int // sequential 主力 key 下标（stickyMu 保护；并发下指针竞态只影响主力偏好，不影响正确性）
	stickyMu sync.Mutex
}

// keyPoolOpts newKeyPool 的具名参数（避免位置参数随配置增长）
type keyPoolOpts struct {
	Keys         []*GeminiKey
	KeyCooldown  time.Duration
	BanAfterFail int32
	MaxRPM       int
	MinInterval  time.Duration
	Strategy     string        // round_robin（默认）| sequential
	DailyLimit   int           // 0=不限制
	ResetLoc     *time.Location // nil=UTC
}

func newKeyPool(o keyPoolOpts) *KeyPool {
	if o.Strategy != strategySequential {
		o.Strategy = strategyRoundRobin
	}
	if o.ResetLoc == nil {
		o.ResetLoc = time.UTC
	}
	return &KeyPool{
		keys:         o.Keys,
		keyCooldown:  o.KeyCooldown,
		banAfterFail: o.BanAfterFail,
		maxRPM:       o.MaxRPM,
		minInterval:  o.MinInterval,
		strategy:     o.Strategy,
		dailyLimit:   o.DailyLimit,
		resetLoc:     o.ResetLoc,
	}
}

func (kp *KeyPool) Len() int { return len(kp.keys) }

// GetNextKey 按策略选中一个可用 key 并占用（inFlight+1）：
//   - round_robin（默认）：原子轮询一圈，兼容旧行为
//   - sequential：优先主力 key（sticky），硬性不可用（冷却/禁用/配额耗尽/限速）才顺移到下一个，指针不回退
//
// chargeDaily 表示该请求是否消耗 model 的每日生成配额（countTokens 类端点传 false：
// 不计数、不受耗尽限制）。调用方必须在请求结束（含长流关闭/超时）时 ReleaseKey 归还，
// 否则长流期间该 key 不会被并发选中。
func (kp *KeyPool) GetNextKey(model string, chargeDaily bool) (*GeminiKey, error) {
	total := len(kp.keys)
	if total == 0 {
		return nil, errors.New("节点无可用 key（api_keys 为空）")
	}
	now := time.Now().In(kp.resetLoc)
	if kp.strategy == strategySequential {
		return kp.getSequential(now, model, chargeDaily, total)
	}
	for i := 0; i < total; i++ {
		idx := int(atomic.AddUint64(&kp.index, 1)) % total
		k := kp.keys[idx]
		if !kp.keyAvailable(k, now, model, chargeDaily) {
			continue
		}
		if k.tryAcquire(now, model, kp.dailyLimit, chargeDaily) {
			return k, nil
		}
		// CAS 抢占失败（并发下另一协程刚占用）→ 换下一个 key
	}
	return nil, ErrAllKeysUnavailable
}

// getSequential 顺序消耗策略：主力 key（sticky）硬性可用则优先——包括仅在途占用的情形，
// 此时本次请求借用其他 key 但不迁移主力指针，主力空闲后自动回归（否则一条长流会把
// 主力永久顺移，并发流式负载下 sequential 退化为只向前轮询）。
// 主力硬性不可用（冷却/禁用/配额耗尽/限速）才顺移指针，且不回退。
func (kp *KeyPool) getSequential(now time.Time, model string, chargeDaily bool, total int) (*GeminiKey, error) {
	kp.stickyMu.Lock()
	start := kp.sticky
	kp.stickyMu.Unlock()

	stickyHardOK := kp.keyHardAvailable(kp.keys[start], now, model, chargeDaily)
	for i := 0; i < total; i++ {
		idx := (start + i) % total
		k := kp.keys[idx]
		if !kp.keyHardAvailable(k, now, model, chargeDaily) {
			continue
		}
		if !k.tryAcquire(now, model, kp.dailyLimit, chargeDaily) {
			continue // 该候选仅在途被占用（CAS 失败）→ 试下一个
		}
		if !stickyHardOK {
			kp.stickyMu.Lock()
			kp.sticky = idx
			kp.stickyMu.Unlock()
		}
		return k, nil
	}
	return nil, ErrAllKeysUnavailable
}

// keyHardAvailable 硬性可用性：状态/RPM/最小间隔/每日配额，不含在途占用
// （sequential 的主力 key 仅在途不算不可用；占用互斥由 tryAcquire 的 CAS 保证）
func (kp *KeyPool) keyHardAvailable(k *GeminiKey, now time.Time, model string, chargeDaily bool) bool {
	if k.effectiveStatus(now) != KeyStatusActive {
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
	if chargeDaily && k.modelQuotaExhausted(now, model, kp.dailyLimit) {
		return false
	}
	return true
}

// keyAvailable 完整可用性：硬性可用且当前无在途占用（round_robin 轮询口径）
func (kp *KeyPool) keyAvailable(k *GeminiKey, now time.Time, model string, chargeDaily bool) bool {
	if k.inFlight.Load() > 0 {
		return false
	}
	return kp.keyHardAvailable(k, now, model, chargeDaily)
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

// ReadyCountForModel 当前 Active 且对 model 当日配额未耗尽的 key 数
func (kp *KeyPool) ReadyCountForModel(model string) int {
	now := time.Now().In(kp.resetLoc)
	n := 0
	for _, k := range kp.keys {
		if k.effectiveStatus(now) != KeyStatusActive {
			continue
		}
		if k.modelQuotaExhausted(now, model, kp.dailyLimit) {
			continue
		}
		n++
	}
	return n
}

// Report200 请求成功：清零连续失败计数
func (kp *KeyPool) Report200(k *GeminiKey) {
	k.Status200.Add(1)
	k.consecFailures.Store(0)
}

// Report429 分钟级限流（RPM/TPM/未知型 429）：冷却该 key。
// backoff>0 时优先采用（来自 Google RetryInfo.retryDelay，受 max429Backoff 上限保护）；
// 否则回退 keyCooldown——用户显式配置的冷却时长不参与钳制（配 2h 就冷却 2h，
// 钳制只针对上游给的重试延迟，避免静默改变显式配置）。
func (kp *KeyPool) Report429(k *GeminiKey, backoff time.Duration) {
	k.Status429.Add(1)
	k.recordError("429 rate limited")
	if backoff > max429Backoff {
		backoff = max429Backoff // 异常大的 retryDelay 不至于长时间锁死 key（每日型走 Report429Daily）
	}
	if backoff <= 0 {
		backoff = kp.keyCooldown
	}
	k.Cool(backoff)
}

// max429Backoff retryDelay 型冷却的上限保护
const max429Backoff = 30 * time.Minute

// Report429Daily 每日配额（RPD）型 429：该 key 对 model 标记耗尽到次日重置时刻，到点自愈
func (kp *KeyPool) Report429Daily(k *GeminiKey, model string) {
	k.Status429.Add(1)
	k.recordError("429 daily quota exhausted: " + model)
	k.markModelExhausted(model, kp.resetLoc)
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

// DailyUsageSnapshot admin 快照中的单模型当日用量
type DailyUsageSnapshot struct {
	Used           int    `json:"used"`
	Limit          int    `json:"limit"`
	ExhaustedUntil string `json:"exhausted_until,omitempty"`
}

// KeySnapshot admin 快照中的单 key 状态（绝不输出完整 key）
type KeySnapshot struct {
	ID            string                       `json:"id"`
	Status        string                       `json:"status"`
	InFlight      int32                        `json:"in_flight"`
	TotalRequests uint64                       `json:"total_requests"`
	Status200     uint64                       `json:"status_200"`
	Status4xx     uint64                       `json:"status_4xx"`
	Status5xx     uint64                       `json:"status_5xx"`
	Status429     uint64                       `json:"status_429"`
	BannedReason  string                       `json:"banned_reason,omitempty"`
	LastError     string                       `json:"last_error,omitempty"`
	CoolingUntil  string                       `json:"cooling_until,omitempty"`
	LastUsedAt    string                       `json:"last_used_at,omitempty"`
	DailyUsage    map[string]DailyUsageSnapshot `json:"daily_usage,omitempty"`
}

func (kp *KeyPool) Snapshots() []KeySnapshot {
	out := make([]KeySnapshot, 0, len(kp.keys))
	now := time.Now().In(kp.resetLoc)
	today := now.Format("2006-01-02")
	for _, k := range kp.keys {
		k.mu.Lock()
		breason := k.bannedReason
		lerr := k.lastError
		coolingUntil := k.coolingUntil
		lastUsed := k.lastUsedAt
		var daily map[string]DailyUsageSnapshot
		for model, u := range k.modelDay {
			if u.day != today || u.used <= 0 {
				continue // 跨天残留条目不展示（下次计数时自动清零）
			}
			if daily == nil {
				daily = map[string]DailyUsageSnapshot{}
			}
			ds := DailyUsageSnapshot{Used: u.used, Limit: kp.dailyLimit}
			if u.exhaustedUntil > 0 {
				ds.ExhaustedUntil = time.Unix(0, u.exhaustedUntil).In(kp.resetLoc).Format("01-02 15:04:05")
			}
			daily[model] = ds
		}
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
			DailyUsage:    daily,
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
