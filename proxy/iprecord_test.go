package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractPublicIP(t *testing.T) {
	cases := []struct {
		name, output, want string
	}{
		{
			name:   "success line preferred",
			output: "当前旧公网 IP: 1.2.3.4\n[SUCCESS] 换 IP 成功！新公网 IP: 5.6.7.8\n",
			want:   "5.6.7.8",
		},
		{
			name:   "fallback last ipv4",
			output: "Private IP: 10.0.0.1\n旧公网 IP: 1.2.3.4\n新 IP 生效\n9.9.9.9\n",
			want:   "9.9.9.9",
		},
		{
			name:   "no ip",
			output: "换 IP 失败: timeout",
			want:   "",
		},
		{
			name:   "empty output",
			output: "",
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractPublicIP(c.output); got != c.want {
				t.Fatalf("extractPublicIP(%q) = %q, want %q", c.output, got, c.want)
			}
		})
	}
}

func TestPersistIPRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip_record.txt")

	persistIPRecord(path, "vps-node-sg-2", "5.6.7.8")
	persistIPRecord(path, "vps-node-jp2", "9.9.9.9")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), data)
	}
	// 节点名排序稳定
	if !strings.HasPrefix(lines[0], "vps-node-jp2 ") || !strings.HasPrefix(lines[1], "vps-node-sg-2 ") {
		t.Fatalf("unexpected lines: %q", data)
	}

	// 更新一个节点，另一个保留
	persistIPRecord(path, "vps-node-jp2", "8.8.8.8")
	data, _ = os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "vps-node-jp2 8.8.8.8") {
		t.Fatalf("updated ip missing: %q", content)
	}
	if !strings.Contains(content, "vps-node-sg-2 5.6.7.8") {
		t.Fatalf("other node record lost: %q", content)
	}
}
