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

func TestMaskKeyID(t *testing.T) {
	if id := maskKeyID("AIzaSy0123456789abcdef"); id != "AIzaSy...cdef" {
		t.Fatalf("maskKeyID = %q", id)
	}
	if id := maskKeyID("short"); id != "***" {
		t.Fatalf("短 key 应全脱敏: %q", id)
	}
}

// GetNextKey 会占用 in-flight；测试里取一个即归还，避免互相阻塞
func takeRelease(kp *KeyPool) (*GeminiKey, error) {
	k, err := kp.GetNextKey()
	if err == nil {
		kp.ReleaseKey(k)
	}
	return k, err
}

func TestKeyPool_RoundRobinSkipsCooling(t *testing.T) {
	kp := newKeyPool(testKeys(3, "rr"), time.Millisecond, 3, 0, 0)
	kp.keys[0].Cool(50 * time.Millisecond)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		k, err := takeRelease(kp)
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
	kp := newKeyPool([]*GeminiKey{k}, 20*time.Millisecond, 3, 0, 0)

	k.Cool(20 * time.Millisecond)
	if k.Status() != KeyStatusCooling {
		t.Fatalf("冷却后状态 = %v", k.Status())
	}
	time.Sleep(60 * time.Millisecond) // 冷却 + 抖动(≤+10ms) 必然过期
	if k.Status() != KeyStatusActive {
		t.Fatalf("冷却到期应自动恢复 Active，得到 %v", k.Status())
	}
	if _, err := takeRelease(kp); err != nil {
		t.Fatalf("恢复后应可获取: %v", err)
	}
}

func TestKeyPool_ConsecutiveFailuresBan(t *testing.T) {
	k := newGeminiKey("AIzaSy-ban")
	kp := newKeyPool([]*GeminiKey{k}, time.Millisecond, 3, 0, 0)

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
	if _, err := takeRelease(kp); err == nil {
		t.Fatalf("Banned key 不应再被选中")
	}
}

func TestKeyPool_RPM(t *testing.T) {
	k := newGeminiKey("AIzaSy-rpm")
	kp := newKeyPool([]*GeminiKey{k}, time.Millisecond, 3, 2, 0)

	for i := 0; i < 2; i++ {
		if _, err := takeRelease(kp); err != nil {
			t.Fatalf("第 %d 次应可获取: %v", i, err)
		}
	}
	if _, err := takeRelease(kp); err != ErrAllKeysUnavailable {
		t.Fatalf("超过 maxRPM 应返回 ErrAllKeysUnavailable，得到 %v", err)
	}
}

func TestKeyPool_MinInterval(t *testing.T) {
	k := newGeminiKey("AIzaSy-min")
	kp := newKeyPool([]*GeminiKey{k}, time.Millisecond, 3, 0, 15*time.Millisecond)

	if _, err := takeRelease(kp); err != nil {
		t.Fatalf("首次获取失败: %v", err)
	}
	if _, err := takeRelease(kp); err != ErrAllKeysUnavailable {
		t.Fatalf("min_key_interval 内应跳过同 key，得到 %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := takeRelease(kp); err != nil {
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

// TestKeyPool_ConcurrentInFlight 并发压测：GetNextKey 命中即占用，任何时刻同一 key
// 至多被一个 goroutine 持有（长流期间不会被并发选中）。
func TestKeyPool_ConcurrentInFlight(t *testing.T) {
	keys := testKeys(8, "conc")
	kp := newKeyPool(keys, time.Millisecond, 3, 0, 0)

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
					kk, err := kp.GetNextKey()
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
		t.Fatalf("存在同 key 被并发同时占用 %d 次", violations)
	}
	if holds == 0 {
		t.Fatalf("并发下应能持续获取 key")
	}
	// 全部归还后，TotalRequests 应等于成功获取次数
	var total uint64
	for _, k := range keys {
		total += k.TotalRequests.Load()
	}
	if total != uint64(holds) {
		t.Fatalf("TotalRequests=%d != holds=%d", total, holds)
	}
	if kp.ReadyCount() != len(keys) {
		t.Fatalf("结束后应全部可用，ReadyCount=%d", kp.ReadyCount())
	}
}