package gemini

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "time/tzdata" // 内嵌时区库：Windows 开发机/裸 linux-arm64 无系统 tzdata 时 LoadLocation 仍可用（测试二进制同样受益）
)

// dailyUsageFlushInterval 后台落盘检查周期
const dailyUsageFlushInterval = 5 * time.Second

// dailyUsageEntry 单 key 单模型的当日用量（持久化格式）
type dailyUsageEntry struct {
	Used           int    `json:"used"`
	ExhaustedUntil string `json:"exhausted_until,omitempty"` // RFC3339Nano，空=未耗尽
}

// dailyUsageFileLayout 持久化文件结构：key 用 sha256 前 32 hex，文件不落明文 key
type dailyUsageFileLayout struct {
	Day  string                                `json:"day"`
	Keys map[string]map[string]dailyUsageEntry `json:"keys"`
}

// dailyUsageStore Gemini 每日用量持久化（Pool 级）：周期全量序列化，
// 内容有变化才原子落盘（临时文件 + rename），未变化零写入。
type dailyUsageStore struct {
	path string
	loc  *time.Location

	nodes []*GeminiNode

	mu       sync.Mutex
	lastData []byte // 上次落盘内容（去重用）

	stop chan struct{}
}

func newDailyUsageStore(path string, loc *time.Location) *dailyUsageStore {
	return &dailyUsageStore{path: path, loc: loc, stop: make(chan struct{})}
}

// hashKeyID key 的持久化标识：sha256 前 32 hex（避免文件落明文 key）
func hashKeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:32]
}

// attach 绑定需要遍历的节点集合（落盘/恢复范围）
func (s *dailyUsageStore) attach(nodes []*GeminiNode) { s.nodes = nodes }

// restore 启动时恢复当日计数：文件 day 等于今天（重置时区）才灌回，否则忽略（跨天自动作废）
func (s *dailyUsageStore) restore() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[gemini] 读取每日用量文件 %s 失败: %v", s.path, err)
		}
		return
	}
	var f dailyUsageFileLayout
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("[gemini] 每日用量文件 %s 损坏，忽略: %v", s.path, err)
		return
	}
	today := time.Now().In(s.loc).Format("2006-01-02")
	if f.Day != today {
		log.Printf("[gemini] 每日用量文件为 %s（今天是 %s），跨天作废不恢复", f.Day, today)
		return
	}
	restored := 0
	for _, n := range s.nodes {
		for _, k := range n.keyPool.keys {
			entries, ok := f.Keys[hashKeyID(k.key)]
			if !ok {
				continue
			}
			for model, e := range entries {
				k.restoreDailyUsage(today, model, e.Used, e.ExhaustedUntil)
				restored++
			}
		}
	}
	// 恢复后先记下当前序列化结果，避免启动后立即重复写等价内容
	s.lastData = s.serialize(time.Now().In(s.loc))
	log.Printf("[gemini] 已从 %s 恢复 %d 条当日用量记录", s.path, restored)
}

// start 启动后台落盘协程
func (s *dailyUsageStore) start() { go s.flusherLoop() }

func (s *dailyUsageStore) flusherLoop() {
	t := time.NewTicker(dailyUsageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.writeIfChanged()
		}
	}
}

// writeIfChanged 序列化当前用量，与上次落盘内容不同才写（同步，优雅关闭时复用）。
// 全程持 s.mu：serialize 与落盘串行化——否则周期 ticker 与 FlushDailyUsage 并发时
// （典型：Shutdown 10s 超时后在途长流仍在计数），慢的一方会用旧数据覆盖新落盘内容。
// 锁序为 s.mu → k.mu（serialize 持各 key 的 k.mu），无反向路径，不会死锁。
func (s *dailyUsageStore) writeIfChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.serialize(time.Now().In(s.loc))
	if len(data) == 0 {
		return // 今日无任何计数，不覆盖旧文件（旧内容由 restore 的 day 校验自动作废）
	}
	if bytes.Equal(data, s.lastData) {
		return
	}
	if err := atomicWriteFile(s.path, data); err != nil {
		log.Printf("[gemini] 每日用量落盘失败: %v", err)
		return
	}
	s.lastData = data
}

// serialize 全量序列化当日用量（只导出今天且有计数的条目；同 key 误配多节点时取 max，保守方向）
func (s *dailyUsageStore) serialize(now time.Time) []byte {
	today := now.Format("2006-01-02")
	type mergeEntry struct {
		used  int
		until int64
	}
	merged := map[string]map[string]mergeEntry{}
	for _, n := range s.nodes {
		for _, k := range n.keyPool.keys {
			k.mu.Lock()
			for model, u := range k.modelDay {
				if u.day != today || u.used <= 0 {
					continue
				}
				id := hashKeyID(k.key)
				if merged[id] == nil {
					merged[id] = map[string]mergeEntry{}
				}
				e := mergeEntry{used: u.used, until: u.exhaustedUntil}
				if prev, ok := merged[id][model]; ok {
					if prev.used > e.used {
						e.used = prev.used
					}
					if prev.until > e.until {
						e.until = prev.until
					}
				}
				merged[id][model] = e
			}
			k.mu.Unlock()
		}
	}
	if len(merged) == 0 {
		return nil
	}
	f := dailyUsageFileLayout{Day: today, Keys: map[string]map[string]dailyUsageEntry{}}
	for id, models := range merged {
		f.Keys[id] = map[string]dailyUsageEntry{}
		for model, e := range models {
			entry := dailyUsageEntry{Used: e.used}
			if e.until > 0 {
				entry.ExhaustedUntil = time.Unix(0, e.until).In(s.loc).Format(time.RFC3339Nano)
			}
			f.Keys[id][model] = entry
		}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	return b
}

// atomicWriteFile 临时文件 + rename 原子落盘，避免写一半被读到
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gemini_daily_usage_*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
