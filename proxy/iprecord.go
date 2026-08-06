package proxy

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ipRecordFile 公网 IP 记录文件（当前工作目录），一节点一行
const ipRecordFile = "ip_record.txt"

// ipRecordMu 串行化 IP 记录文件的读写，防止多节点并发换 IP 时写坏文件
var ipRecordMu sync.Mutex

var successIPRe = regexp.MustCompile(`新公网 IP:\s*([0-9]{1,3}(?:\.[0-9]{1,3}){3})`)
var anyIPv4Re = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`)

// extractPublicIP 从换 IP 脚本输出中提取新公网 IP：
// 优先匹配脚本 SUCCESS 行的"新公网 IP: X"，未命中则回退取输出中最后一个 IPv4
func extractPublicIP(output string) string {
	if m := successIPRe.FindStringSubmatch(output); m != nil {
		return m[1]
	}
	matches := anyIPv4Re.FindAllString(output, -1)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	return ""
}

// persistIPRecord 将节点最新 IP 记录到文件（一节点一行，全量原子重写）
func persistIPRecord(path, nodeName, ip string) {
	ipRecordMu.Lock()
	defer ipRecordMu.Unlock()

	records := readIPRecords(path)
	records[nodeName] = ip

	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)

	// 写入临时文件后原子 rename，避免写一半被读到
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ip_record_*.tmp")
	if err != nil {
		log.Printf("创建 IP 记录临时文件失败: %v", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	w := bufio.NewWriter(tmp)
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "%s %s\n", name, records[name]); err != nil {
			log.Printf("写入 IP 记录失败: %v", err)
			tmp.Close()
			return
		}
	}
	if err := w.Flush(); err != nil {
		log.Printf("刷盘 IP 记录失败: %v", err)
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("关闭 IP 记录临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		log.Printf("替换 IP 记录文件失败: %v", err)
		return
	}
}

// readIPRecords 读取现有记录文件为 节点名 -> IP 映射
func readIPRecords(path string) map[string]string {
	records := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return records
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			records[fields[0]] = fields[1]
		}
	}
	return records
}
