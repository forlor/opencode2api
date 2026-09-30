package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkUsageNode 构造仅含 keyPool 的裸节点（持久化遍历用）
func mkUsageNode(cooldown time.Duration, dailyLimit int, keys ...string) *GeminiNode {
	gk := make([]*GeminiKey, 0, len(keys))
	for _, raw := range keys {
		gk = append(gk, newGeminiKey(raw))
	}
	return &GeminiNode{keyPool: newKeyPool(keyPoolOpts{
		Keys: gk, KeyCooldown: cooldown, BanAfterFail: 3, DailyLimit: dailyLimit, ResetLoc: time.UTC,
	})}
}

// 写 → 重启恢复 → 计数延续；文件不落明文 key
func TestDailyUsage_PersistAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")

	// 第一次运行：计数 3 次后落盘
	n1 := mkUsageNode(time.Millisecond, 5, "AIzaSy-daily-persist-a")
	s1 := newDailyUsageStore(path, time.UTC)
	s1.attach([]*GeminiNode{n1})
	for i := 0; i < 3; i++ {
		if _, err := takeRelease(n1.keyPool, "gemini-2.5-flash"); err != nil {
			t.Fatal(err)
		}
	}
	s1.writeIfChanged()

	// 文件不落明文 key
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "AIzaSy-daily-persist-a") {
		t.Fatalf("持久化文件泄露明文 key: %s", raw)
	}

	// 重启（同 key 值新实例）：恢复 3 次计数，再取 2 次达到 5 次限额耗尽
	n2 := mkUsageNode(time.Millisecond, 5, "AIzaSy-daily-persist-a")
	s2 := newDailyUsageStore(path, time.UTC)
	s2.attach([]*GeminiNode{n2})
	s2.restore()
	for i := 0; i < 2; i++ {
		if _, err := takeRelease(n2.keyPool, "gemini-2.5-flash"); err != nil {
			t.Fatalf("恢复后第 %d 次应可获取: %v", i+1, err)
		}
	}
	if _, err := takeRelease(n2.keyPool, "gemini-2.5-flash"); err != ErrAllKeysUnavailable {
		t.Fatalf("恢复计数 + 新计数达到限额应耗尽，得到 %v", err)
	}
}

// 跨天文件忽略：文件 day 非今天 → 不恢复
func TestDailyUsage_CrossDayIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	stale, _ := json.Marshal(dailyUsageFileLayout{
		Day:  "2000-01-01",
		Keys: map[string]map[string]dailyUsageEntry{hashKeyID("AIzaSy-stale"): {"m": {Used: 99}}},
	})
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	n := mkUsageNode(time.Millisecond, 5, "AIzaSy-stale")
	s := newDailyUsageStore(path, time.UTC)
	s.attach([]*GeminiNode{n})
	s.restore()
	if _, err := takeRelease(n.keyPool, "m"); err != nil {
		t.Fatalf("跨天文件不应恢复计数: %v", err)
	}
}

// 内容未变化时不重复写盘
func TestDailyUsage_NoChangeNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	n := mkUsageNode(time.Millisecond, 0, "AIzaSy-dedup")
	s := newDailyUsageStore(path, time.UTC)
	s.attach([]*GeminiNode{n})

	if _, err := takeRelease(n.keyPool, "m"); err != nil {
		t.Fatal(err)
	}
	s.writeIfChanged()
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 无新计数 → 第二次落盘应跳过（mtime 不变）
	time.Sleep(10 * time.Millisecond)
	s.writeIfChanged()
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("内容未变化不应重复写盘")
	}
}

// 同一 key 误配多节点：落盘取 max
func TestDailyUsage_DuplicateKeyTakesMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	const dup = "AIzaSy-dup-key-0001"
	na := mkUsageNode(time.Millisecond, 10, dup)
	nb := mkUsageNode(time.Millisecond, 10, dup)
	for i := 0; i < 2; i++ {
		if _, err := takeRelease(na.keyPool, "m"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if _, err := takeRelease(nb.keyPool, "m"); err != nil {
			t.Fatal(err)
		}
	}
	s := newDailyUsageStore(path, time.UTC)
	s.attach([]*GeminiNode{na, nb})
	s.writeIfChanged()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f dailyUsageFileLayout
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if e := f.Keys[hashKeyID(dup)]["m"]; e.Used != 4 {
		t.Fatalf("同 key 多节点应取 max=4，得到 %d", e.Used)
	}
}
