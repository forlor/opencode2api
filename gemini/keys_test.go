package gemini

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testKeys(n int, prefix string) []*GeminiKey {
	out := make([]*GeminiKey, 0, n)
	for i := 0; i < n; i++ {
		// 尾部后缀区分，保证脱敏 ID 互不相同（否则 maskKeyID 只留前6+后4）
		out = append(out, newGeminiKey(fmt.Sprintf("AIzaSy-%s-%d-0123456789ab%02d", prefix, i, i)))
	}
	return out
}

// 简单 key 池构造（默认 round_robin、UTC、无每日限额）
func simplePool(keys []*GeminiKey, cooldown time.Duration) *KeyPool {
	return newKeyPool(keyPoolOpts{Keys: keys, KeyCooldown: cooldown, BanAfterFail: 3})
}

func TestMaskKeyID(t *testing.T) {
	if id := maskKeyID("AIzaSy0123456789abcdef"); id != "AIzaSy...cdef" {
		t.Fatalf("maskKeyID = %q", id)
	}
	if id := maskKeyID("short"); id != "***" {
		t.Fatalf("短 key 应全脱敏: %q", id)
	}
}

// GetNextKey 会占用 in-flight；测试里取一个即归还，避免互相阻塞
func takeRelease(kp *KeyPool, model string) (*GeminiKey, error) {
	return takeReleaseCharge(kp, model, true)
}

// takeReleaseCharge 支持指定 chargeDaily（countTokens 类端点传 false）
func takeReleaseCharge(kp *KeyPool, model string, chargeDaily bool) (*GeminiKey, error) {
	k, err := kp.GetNextKey(model, chargeDaily)
	if err == nil {
		kp.ReleaseKey(k)
	}
	return k, err
}

func TestKeyPool_RoundRobinSkipsCooling(t *testing.T) {
	kp := simplePool(testKeys(3, "rr"), time.Millisecond)
	kp.keys[0].Cool(50 * time.Millisecond)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		k, err := takeRelease(kp, "")
		if err != nil {
			t.Fatalf("第 %d 次获取失败: %v", i, err)
		}
		if k.ID() == kp.keys[0].ID() {
			t.Fatalf("冷却中的 key[0] 不应被命中")
		}
		seen[k.ID()] = true
	}
	// 其余两个 key 都应被轮询到
	if !seen[kp.keys[1].ID()] || !seen[kp.keys[2].ID()] {
		t.Fatalf("未覆盖全部可用 key: %v", seen)
	}
}

func TestKey_CoolAutoRecovery(t *testing.T) {
	k := newGeminiKey("AIzaSy-auto")
	kp := simplePool([]*GeminiKey{k}, 20*time.Millisecond)

	k.Cool(20 * time.Millisecond)
	if k.Status() != KeyStatusCooling {
		t.Fatalf("冷却后状态 = %v", k.Status())
	}
	time.Sleep(60 * time.Millisecond) // 冷却 + 抖动(≤+10ms) 必然过期
	if k.Status() != KeyStatusActive {
		t.Fatalf("冷却到期应自动恢复 Active，得到 %v", k.Status())
	}
	if _, err := takeRelease(kp, ""); err != nil {
		t.Fatalf("恢复后应可获取: %v", err)
	}
}

func TestKeyPool_ConsecutiveFailuresBan(t *testing.T) {
	k := newGeminiKey("AIzaSy-ban")
	kp := simplePool([]*GeminiKey{k}, time.Millisecond)

	// 连续 5xx 2 次：仍未 Ban
	kp.Report5xx(k)
	kp.Report5xx(k)
	if k.Status() != KeyStatusActive {
		t.Fatalf("2 次失败不应 Ban，得到 %v", k.Status())
	}
	// 第 3 次 → Ban
	kp.Report5xx(k)
	if k.Status() != KeyStatusBanned {
		t.Fatalf("3 次失败应 Ban，得到 %v", k.Status())
	}
	if _, err := takeRelease(kp, ""); err == nil {
		t.Fatalf("Banned key 不应再被选中")
	}
}

func TestKeyPool_RPM(t *testing.T) {
	k := newGeminiKey("AIzaSy-rpm")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, MaxRPM: 2})

	for i := 0; i < 2; i++ {
		if _, err := takeRelease(kp, ""); err != nil {
			t.Fatalf("第 %d 次应可获取: %v", i, err)
		}
	}
	if _, err := takeRelease(kp, ""); err != ErrAllKeysUnavailable {
		t.Fatalf("超过 maxRPM 应返回 ErrAllKeysUnavailable，得到 %v", err)
	}
}

func TestKeyPool_MinInterval(t *testing.T) {
	k := newGeminiKey("AIzaSy-min")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, MinInterval: 15 * time.Millisecond})

	if _, err := takeRelease(kp, ""); err != nil {
		t.Fatalf("首次获取失败: %v", err)
	}
	if _, err := takeRelease(kp, ""); err != ErrAllKeysUnavailable {
		t.Fatalf("min_key_interval 内应跳过同 key，得到 %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := takeRelease(kp, ""); err != nil {
		t.Fatalf("min_key_interval 过后应可再取: %v", err)
	}
}

func TestKeyPool_Unban(t *testing.T) {
	k := newGeminiKey("AIzaSy-unban")
	k.Ban("test")
	if k.Status() != KeyStatusBanned {
		t.Fatalf("状态 = %v", k.Status())
	}
	k.Unban()
	if k.Status() != KeyStatusActive {
		t.Fatalf("Unban 后应 Active，得到 %v", k.Status())
	}
	if k.bannedReasonText() != "" {
		t.Fatalf("Unban 应清空 bannedReason: %q", k.bannedReasonText())
	}
}

// ==================== sequential 策略 ====================

// sequential：无干扰时持续命中主力 key（指针不动）；主力冷却后顺移，冷却结束指针不回退
func TestKeyPool_SequentialSticky(t *testing.T) {
	kp := newKeyPool(keyPoolOpts{Keys: testKeys(3, "seq"), KeyCooldown: time.Millisecond, BanAfterFail: 3, Strategy: "sequential"})

	for i := 0; i < 5; i++ {
		k, err := takeRelease(kp, "m")
		if err != nil {
			t.Fatalf("第 %d 次获取失败: %v", i, err)
		}
		if k != kp.keys[0] {
			t.Fatalf("sequential 应持续命中主力 key[0]，得到 %s", k.ID())
		}
	}

	// 主力 key 冷却 → 顺移到 key[1]
	kp.keys[0].CoolUntil(50 * time.Millisecond)
	k, err := takeRelease(kp, "m")
	if err != nil {
		t.Fatal(err)
	}
	if k != kp.keys[1] {
		t.Fatalf("主力冷却应顺移到 key[1]，得到 %s", k.ID())
	}

	// key[0] 冷却结束，但指针不回退：仍命中 key[1]
	time.Sleep(100 * time.Millisecond)
	k, err = takeRelease(kp, "m")
	if err != nil {
		t.Fatal(err)
	}
	if k != kp.keys[1] {
		t.Fatalf("sequential 指针不回退，应仍命中 key[1]，得到 %s", k.ID())
	}
}

// sequential + 每日配额：主力 key 对该模型耗尽后顺移；全部耗尽报错
func TestKeyPool_SequentialDailyExhaustMoves(t *testing.T) {
	keys := testKeys(2, "seqd")
	kp := newKeyPool(keyPoolOpts{Keys: keys, KeyCooldown: time.Millisecond, BanAfterFail: 3, Strategy: "sequential", DailyLimit: 1})

	a, err := takeRelease(kp, "m")
	if err != nil || a != keys[0] {
		t.Fatalf("首次应命中 key[0]: %v %v", a, err)
	}
	b, err := takeRelease(kp, "m")
	if err != nil || b != keys[1] {
		t.Fatalf("key[0] 配额耗尽应顺移到 key[1]: %v %v", b, err)
	}
	if _, err := takeRelease(kp, "m"); err != ErrAllKeysUnavailable {
		t.Fatalf("全部 key 耗尽应报 ErrAllKeysUnavailable，得到 %v", err)
	}
}

// ==================== 每日配额 ====================

// dailyLimit 达标后该 key 对该模型不再被选中；模型之间独立计数
func TestKeyPool_DailyLimitPerModel(t *testing.T) {
	k := newGeminiKey("AIzaSy-daily-pm")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 2})

	for i := 0; i < 2; i++ {
		if _, err := takeRelease(kp, "gemini-2.5-flash"); err != nil {
			t.Fatalf("第 %d 次应可获取: %v", i+1, err)
		}
	}
	if _, err := takeRelease(kp, "gemini-2.5-flash"); err != ErrAllKeysUnavailable {
		t.Fatalf("达到 daily_limit 应不可再取，得到 %v", err)
	}
	// 模型 B 独立计数，不受模型 A 耗尽影响
	if _, err := takeRelease(kp, "gemini-2.5-pro"); err != nil {
		t.Fatalf("per-model 语义：模型 A 耗尽不应影响模型 B: %v", err)
	}
	if kp.ReadyCountForModel("gemini-2.5-flash") != 0 {
		t.Fatalf("模型 A 的 ReadyCountForModel 应为 0")
	}
	if kp.ReadyCountForModel("gemini-2.5-pro") != 1 {
		t.Fatalf("模型 B 的 ReadyCountForModel 应为 1")
	}
	// round_robin 下其他 key 对模型 A 仍可用
	kp2 := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k, newGeminiKey("AIzaSy-daily-p2")}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 2})
	if _, err := takeRelease(kp2, "gemini-2.5-flash"); err != nil {
		t.Fatalf("耗尽只影响单个 key: %v", err)
	}
}

// exhaustedUntil 到期惰性自愈：耗尽标记过期后该模型恢复可用并清零计数
func TestKey_DailyExhaustedSelfHeal(t *testing.T) {
	k := newGeminiKey("AIzaSy-heal")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 2})

	// 直接置一个已过期的耗尽标记（模拟跨过重置时刻）
	k.mu.Lock()
	k.modelDay["m"] = &modelDayUsage{
		day:            time.Now().In(kp.resetLoc).Format("2006-01-02"),
		used:           2,
		exhaustedUntil: time.Now().Add(-time.Second).UnixNano(),
	}
	k.mu.Unlock()
	if _, err := takeRelease(kp, "m"); err != nil {
		t.Fatalf("耗尽标记过期应自愈恢复可用: %v", err)
	}
	// 自愈同时清零计数：可再取一次（第 2 次达到限额后耗尽）
	if _, err := takeRelease(kp, "m"); err != nil {
		t.Fatalf("自愈清零后应可继续计数: %v", err)
	}
	if _, err := takeRelease(kp, "m"); err != ErrAllKeysUnavailable {
		t.Fatalf("清零后重新计数到达限额应耗尽，得到 %v", err)
	}
}

// Report429Daily：该 key 该模型标记耗尽到次日重置；key 状态保持 Active（per-model 不用 key 级 Cooling）
func TestKeyPool_Report429Daily(t *testing.T) {
	k := newGeminiKey("AIzaSy-d429")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 5})

	kp.Report429Daily(k, "m")
	if _, err := takeRelease(kp, "m"); err != ErrAllKeysUnavailable {
		t.Fatalf("daily 429 后该模型应耗尽，得到 %v", err)
	}
	if _, err := takeRelease(kp, "n"); err != nil {
		t.Fatalf("其他模型不受影响: %v", err)
	}
	if k.Status() != KeyStatusActive {
		t.Fatalf("per-model 耗尽不应进入 key 级 Cooling，状态 = %v", k.Status())
	}
	// 标记的重置时刻应为重置时区次日 00:00
	k.mu.Lock()
	until := k.modelDay["m"].exhaustedUntil
	k.mu.Unlock()
	want := nextDailyReset(time.Now(), kp.resetLoc)
	if d := time.Until(time.Unix(0, until)) - time.Until(want); d > time.Second || d < -time.Second {
		t.Fatalf("耗尽时刻应约为次日重置时刻，偏差 %v", d)
	}
}

// Report429 带 retryDelay 时优先采用，且受上限保护；无 retryDelay 回退 keyCooldown
func TestKeyPool_Report429Backoff(t *testing.T) {
	k := newGeminiKey("AIzaSy-b429")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: 60 * time.Second, BanAfterFail: 3})

	kp.Report429(k, 2*time.Second)
	if k.Status() != KeyStatusCooling {
		t.Fatalf("429 后应冷却: %v", k.Status())
	}
	// 冷却到 ~2s（带抖动 ≤3s），远短于 keyCooldown 60s：等它到期验证确实采用了短退避
	deadline := time.Now().Add(4 * time.Second)
	for k.Status() == KeyStatusCooling && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if k.Status() != KeyStatusActive {
		t.Fatalf("retryDelay 型短冷却应已到期: %v", k.Status())
	}

	// 异常大的 retryDelay 被钳制（钳制基准 + 抖动上限约 45min，24h 则必然超时）
	kp.Report429(k, 24*time.Hour)
	k.mu.Lock()
	until := time.Unix(0, k.coolingUntil)
	k.mu.Unlock()
	if d := time.Until(until); d >= time.Hour {
		t.Fatalf("超限 retryDelay 应钳制到 %v（含抖动），实际冷却剩余 %v", max429Backoff, d)
	}
}

// ==================== 并发 ====================

// TestKeyPool_ConcurrentInFlight 并发压测：GetNextKey 命中即占用，任何时刻同一 key
// 至多被一个 goroutine 持有（长流期间不会被并发选中）。覆盖两种策略。
func TestKeyPool_ConcurrentInFlight(t *testing.T) {
	for _, strategy := range []string{strategyRoundRobin, strategySequential} {
		keys := testKeys(8, "conc"+strategy[:2])
		kp := newKeyPool(keyPoolOpts{Keys: keys, KeyCooldown: time.Millisecond, BanAfterFail: 3, Strategy: strategy})

		var mu sync.Mutex
		held := map[string]bool{}
		var holds, violations int32

		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					// 全部 key 瞬时被占用属正常，spin 等待；其他错误才算失败
					var k *GeminiKey
					for {
						kk, err := kp.GetNextKey("m", true)
						if err == nil {
							k = kk
							break
						}
						if err != ErrAllKeysUnavailable {
							t.Errorf("并发获取失败: %v", err)
							return
						}
						runtime.Gosched()
					}
					mu.Lock()
					if held[k.ID()] {
						violations++
					}
					held[k.ID()] = true
					mu.Unlock()
					atomic.AddInt32(&holds, 1)
					time.Sleep(100 * time.Microsecond) // 模拟占用窗口
					mu.Lock()
					delete(held, k.ID())
					mu.Unlock()
					kp.ReleaseKey(k)
				}
			}()
		}
		wg.Wait()

		if violations > 0 {
			t.Fatalf("[%s] 存在同 key 被并发同时占用 %d 次", strategy, violations)
		}
		if holds == 0 {
			t.Fatalf("[%s] 并发下应能持续获取 key", strategy)
		}
		// 全部归还后，TotalRequests 应等于成功获取次数
		var total uint64
		for _, k := range keys {
			total += k.TotalRequests.Load()
		}
		if total != uint64(holds) {
			t.Fatalf("[%s] TotalRequests=%d != holds=%d", strategy, total, holds)
		}
		if kp.ReadyCount() != len(keys) {
			t.Fatalf("[%s] 结束后应全部可用，ReadyCount=%d", strategy, kp.ReadyCount())
		}
	}
}

// ==================== 评审修复回归 ====================

// K1 回归：daily_limit=0（默认部署）时 daily-429 的耗尽标记同样生效——
// key 不冷却（保持 Active），但该模型会被跳过，否则 sequential 会把整轮重试烧在同一把耗尽 key 上
func TestKeyPool_Daily429MarkedEvenWithoutLimit(t *testing.T) {
	k := newGeminiKey("AIzaSy-k1-nolimit")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3})

	kp.Report429Daily(k, "m")
	if _, err := takeRelease(kp, "m"); err != ErrAllKeysUnavailable {
		t.Fatalf("daily_limit=0 时 daily-429 标记也应使该模型跳过，得到 %v", err)
	}
	if _, err := takeRelease(kp, "n"); err != nil {
		t.Fatalf("其他模型不受影响: %v", err)
	}
	if k.Status() != KeyStatusActive {
		t.Fatalf("per-model 耗尽不应进入 key 级 Cooling，状态 = %v", k.Status())
	}
}

// K2 回归：countTokens 类端点（chargeDaily=false）不消耗每日生成配额，也不受耗尽限制
func TestKeyPool_CountTokensNotCharged(t *testing.T) {
	k := newGeminiKey("AIzaSy-ct")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 1})

	for i := 0; i < 5; i++ {
		if _, err := takeReleaseCharge(kp, "m", false); err != nil {
			t.Fatalf("第 %d 次 countTokens 不应受限: %v", i+1, err)
		}
	}
	// 一次生成请求即达 daily_limit=1 → 该模型生成耗尽
	if _, err := takeReleaseCharge(kp, "m", true); err != nil {
		t.Fatalf("首次生成请求应可用: %v", err)
	}
	if _, err := takeReleaseCharge(kp, "m", true); err != ErrAllKeysUnavailable {
		t.Fatalf("生成配额耗尽应不可再取，得到 %v", err)
	}
	// 生成耗尽后 countTokens 仍可用
	if _, err := takeReleaseCharge(kp, "m", false); err != nil {
		t.Fatalf("countTokens 不应受生成配额耗尽影响: %v", err)
	}
}

// K3 回归：跨过重置午夜到达的 daily-429（条目属于昨天）不封锁新的一天
func TestKey_MidnightStale429NotMarked(t *testing.T) {
	k := newGeminiKey("AIzaSy-mid")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3, DailyLimit: 5})

	yesterday := time.Now().In(kp.resetLoc).Add(-24 * time.Hour).Format("2006-01-02")
	k.mu.Lock()
	k.modelDay["m"] = &modelDayUsage{day: yesterday, used: 3}
	k.mu.Unlock()

	k.markModelExhausted("m", kp.resetLoc)
	k.mu.Lock()
	u := k.modelDay["m"]
	k.mu.Unlock()
	if u.exhaustedUntil != 0 {
		t.Fatalf("昨天的 429 不应封锁今天，exhaustedUntil=%d", u.exhaustedUntil)
	}
	// 新一天正常可用（计数清零重来）
	if _, err := takeRelease(kp, "m"); err != nil {
		t.Fatalf("跨天 429 后该模型应可用: %v", err)
	}
}

// K5 回归：modelDay 条目数有上限（客户端可控模型名不能无界增长）；跨天首次计数全量清扫
func TestKey_ModelDayBoundedAndSwept(t *testing.T) {
	k := newGeminiKey("AIzaSy-bound")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: time.Millisecond, BanAfterFail: 3})

	for i := 0; i < maxTrackedModels+50; i++ {
		if _, err := takeRelease(kp, fmt.Sprintf("junk-model-%d", i)); err != nil {
			t.Fatalf("超限模型名请求仍应正常转发选 key: %v", err)
		}
	}
	k.mu.Lock()
	n := len(k.modelDay)
	k.mu.Unlock()
	if n > maxTrackedModels {
		t.Fatalf("modelDay 条目数 %d 超过上限 %d", n, maxTrackedModels)
	}

	// 模拟跨天：注入过期条目并把 sweepDay 置为昨天，新一天首次计数应顺带清扫
	k.mu.Lock()
	k.modelDay["stale"] = &modelDayUsage{day: "2000-01-01", used: 9}
	k.sweepDay = time.Now().In(kp.resetLoc).Add(-24 * time.Hour).Format("2006-01-02")
	k.mu.Unlock()
	if _, err := takeRelease(kp, "m"); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	_, hasStale := k.modelDay["stale"]
	k.mu.Unlock()
	if hasStale {
		t.Fatal("跨天残留条目应被清扫")
	}
}

// K6 回归：sequential 主力仅在途（长流占用）时借用其他 key 但不迁移主力，主力空闲后回归
func TestKeyPool_SequentialBorrowWithoutAdvance(t *testing.T) {
	kp := newKeyPool(keyPoolOpts{Keys: testKeys(3, "seqb"), KeyCooldown: time.Millisecond, BanAfterFail: 3, Strategy: "sequential"})

	// 占住主力 key[0] 不还（模拟长流）
	first, err := kp.GetNextKey("m", true)
	if err != nil || first != kp.keys[0] {
		t.Fatalf("首次应命中 key[0]: %v %v", first, err)
	}
	// 主力在途：借用 key[1]，但 sticky 不动
	second, err := kp.GetNextKey("m", true)
	if err != nil || second != kp.keys[1] {
		t.Fatalf("主力在途应借用 key[1]: %v %v", second, err)
	}
	kp.stickyMu.Lock()
	st := kp.sticky
	kp.stickyMu.Unlock()
	if st != 0 {
		t.Fatalf("借用不应迁移主力指针，sticky=%d", st)
	}
	// 长流结束归还后：回归主力 key[0]
	kp.ReleaseKey(first)
	kp.ReleaseKey(second)
	third, err := kp.GetNextKey("m", true)
	if err != nil || third != kp.keys[0] {
		t.Fatalf("主力空闲后应回归 key[0]: %v %v", third, err)
	}
	kp.ReleaseKey(third)
}

// K8 回归：用户显式配置的 key_cooldown_duration 不受 30min 钳制（钳制只针对上游 retryDelay）
func TestKeyPool_Report429FallbackFullCooldown(t *testing.T) {
	k := newGeminiKey("AIzaSy-k8")
	kp := newKeyPool(keyPoolOpts{Keys: []*GeminiKey{k}, KeyCooldown: 2 * time.Hour, BanAfterFail: 3})

	kp.Report429(k, 0) // 无 retryDelay → 回退 keyCooldown=2h，不参与钳制
	k.mu.Lock()
	until := time.Unix(0, k.coolingUntil)
	k.mu.Unlock()
	if d := time.Until(until); d < time.Hour {
		t.Fatalf("回退 keyCooldown 应完整生效（约 2h 含抖动），剩余 %v", d)
	}
}
