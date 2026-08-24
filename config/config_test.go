package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestConfig() *Config {
	cfg := &Config{}
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.Port = 8080
	cfg.Server.APIKeys = []string{"sk-1"}
	cfg.Server.Secret = "sec"
	cfg.Default.CooldownDuration = 30 * time.Minute
	cfg.Default.FallbackModel = "deepseek-v4-flash-free"
	cfg.Default.ModelMappings = map[string]string{
		"gpt-4o":        "deepseek-v4-flash-free",
		"gpt-4o-gemini": "gemini-2.5-flash",
	}
	return cfg
}

func TestRouteRequest_GeminiNil_AlwaysOpenCode(t *testing.T) {
	cfg := newTestConfig()
	dec := cfg.RouteRequest("gpt-4o")
	if dec.Line != LineOpenCode {
		t.Fatalf("期望 opencode，得到 %s", dec.Line)
	}
	if dec.TargetModel != "deepseek-v4-flash-free" {
		t.Fatalf("期望 deepseek-v4-flash-free，得到 %s", dec.TargetModel)
	}
	// 未配置的任何客户端模型也走 opencode（走 fallback）
	dec = cfg.RouteRequest("some-unknown-model")
	if dec.Line != LineOpenCode || dec.TargetModel != "deepseek-v4-flash-free" {
		t.Fatalf("未配置 gemini 时未知模型应走 opencode fallback，得到 %+v", dec)
	}
}

func TestRouteRequest_Disabled_AlwaysOpenCode(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{Enabled: false, Models: []string{"gemini-*"}}
	dec := cfg.RouteRequest("gpt-4o-gemini")
	if dec.Line != LineOpenCode {
		t.Fatalf("未启用 gemini 时即使客户端点名 gemini 模型也应走 opencode，得到 %s", dec.Line)
	}
}

func TestRouteRequest_Force_UsesMappingTarget(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{
		Enabled:      true,
		Force:        true,
		FallbackModel: "gemini-2.5-flash",
		Models:       []string{"gemini-*"},
		StreamingMode: "real",
	}
	// gpt-4o-gemini 映射目标 gemini-2.5-flash 命中 Models
	dec := cfg.RouteRequest("gpt-4o-gemini")
	if dec.Line != LineGemini || dec.TargetModel != "gemini-2.5-flash" {
		t.Fatalf("Force 模式应走 gemini 且用映射目标，得到 %+v", dec)
	}
	if dec.Streaming != "real" {
		t.Fatalf("Streaming 应为 real，得到 %s", dec.Streaming)
	}
}

func TestRouteRequest_Force_Fallback(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{
		Enabled:       true,
		Force:         true,
		FallbackModel: "gemini-3-flash-preview",
		Models:        []string{"gemini-*"},
		StreamingMode: "fake",
	}
	// 未映射的客户端模型 → fallback
	dec := cfg.RouteRequest("unknown-model")
	if dec.Line != LineGemini || dec.TargetModel != "gemini-3-flash-preview" {
		t.Fatalf("Force fallback 期望 gemini-3-flash-preview，得到 %+v", dec)
	}
	if dec.Streaming != "fake" {
		t.Fatalf("Streaming 应为 fake，得到 %s", dec.Streaming)
	}
}

func TestRouteRequest_Manual_ClientModelMatches(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{
		Enabled:      true,
		FallbackModel: "gemini-2.5-flash",
		Models:       []string{"gemini-*"},
		StreamingMode: "real",
	}
	// 客户端直接点名 gemini-2.5-flash
	dec := cfg.RouteRequest("gemini-2.5-flash")
	if dec.Line != LineGemini || dec.TargetModel != "gemini-2.5-flash" {
		t.Fatalf("客户端点名 gemini 模型应走 gemini，得到 %+v", dec)
	}
}

func TestRouteRequest_Manual_MappedTargetMatches(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{
		Enabled:      true,
		FallbackModel: "gemini-2.5-flash",
		Models:       []string{"gemini-*"},
		StreamingMode: "real",
	}
	// gpt-4o-gemini 映射目标 gemini-2.5-flash 命中 Models → gemini
	dec := cfg.RouteRequest("gpt-4o-gemini")
	if dec.Line != LineGemini || dec.TargetModel != "gemini-2.5-flash" {
		t.Fatalf("映射目标命中 gemini 应走 gemini，得到 %+v", dec)
	}
	// gpt-4o 映射目标 deepseek → 仍走 opencode（deepseek 线路不受影响）
	dec = cfg.RouteRequest("gpt-4o")
	if dec.Line != LineOpenCode || dec.TargetModel != "deepseek-v4-flash-free" {
		t.Fatalf("映射目标为 deepseek 应走 opencode，得到 %+v", dec)
	}
}

func TestRouteRequest_Manual_ClientModels(t *testing.T) {
	cfg := newTestConfig()
	cfg.Gemini = &GeminiConfig{
		Enabled:      true,
		FallbackModel: "gemini-2.5-flash",
		Models:       []string{"gemini-*"},
		ClientModels: []string{"gemini-pro-custom"},
		StreamingMode: "real",
	}
	dec := cfg.RouteRequest("gemini-pro-custom")
	if dec.Line != LineGemini || dec.TargetModel != "gemini-pro-custom" {
		t.Fatalf("client_models 精确命中应走 gemini，得到 %+v", dec)
	}
}

func TestMatchModel(t *testing.T) {
	patterns := []string{"gemini-*", "gemma-4-26b-a4b-it"}
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-2.5-flash", true},
		{"gemini-3-flash-preview", true},
		{"gemma-4-26b-a4b-it", true},
		{"gemma-4-27b", false},
		{"deepseek-v4-flash-free", false},
		{"", false},
	}
	for _, c := range cases {
		if got := MatchModel(c.model, patterns); got != c.want {
			t.Fatalf("MatchModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
	if MatchModel("gemini-x", nil) {
		t.Fatal("空列表应返回 false")
	}
}

func TestLoadConfig_GeminiParsing(t *testing.T) {
	yamlContent := `
server:
  host: "0.0.0.0"
  port: 22579
  api_keys: ["sk-1"]
  secret: "sec"
default:
  cooldown_duration: "30m"
  fallback_model: "deepseek-v4-flash-free"
  model_mappings:
    "gpt-4o": "deepseek-v4-flash-free"
    "gpt-4o-gemini": "gemini-2.5-flash"
gemini:
  enabled: true
  fallback_model: "gemini-2.5-flash"
  models: ["gemini-*"]
  streaming_mode: "fake"
  key_cooldown_duration: "90s"
  ban_after_failures: 5
  nodes:
    - name: "gem-vps-sg"
      lan_url: "http://100.96.0.11:22578"
      api_keys: ["AIzaSyAAA", "AIzaSyBBB"]
      max_rpm: 60
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gemini == nil {
		t.Fatal("gemini 应为非 nil")
	}
	if !cfg.Gemini.Enabled {
		t.Fatal("enabled 应为 true")
	}
	if cfg.Gemini.StreamingMode != "fake" {
		t.Fatalf("streaming_mode 应为 fake，得到 %s", cfg.Gemini.StreamingMode)
	}
	if cfg.Gemini.KeyCooldownDuration != 90*time.Second {
		t.Fatalf("key_cooldown_duration 应为 90s，得到 %v", cfg.Gemini.KeyCooldownDuration)
	}
	if cfg.Gemini.BanAfterFailures != 5 {
		t.Fatalf("ban_after_failures 应为 5，得到 %d", cfg.Gemini.BanAfterFailures)
	}
	if cfg.Gemini.MountPath != "/gemini" {
		t.Fatalf("mount_path 默认应为 /gemini，得到 %q", cfg.Gemini.MountPath)
	}
	if cfg.Gemini.DefaultMaxOutputTokens != 8192 {
		t.Fatalf("default_max_output_tokens 默认应为 8192，得到 %d", cfg.Gemini.DefaultMaxOutputTokens)
	}
	if len(cfg.Gemini.Nodes) != 1 {
		t.Fatalf("应有 1 个 gemini 节点，得到 %d", len(cfg.Gemini.Nodes))
	}
	node := cfg.Gemini.Nodes[0]
	if len(node.APIKeys) != 2 {
		t.Fatalf("节点应绑定 2 个 key，得到 %d", len(node.APIKeys))
	}
	if node.MaxRPM != 60 {
		t.Fatalf("节点 max_rpm 应为 60，得到 %d", node.MaxRPM)
	}
	if node.KeyCooldownDuration != 90*time.Second {
		t.Fatalf("节点应继承全局 key_cooldown_duration=90s，得到 %v", node.KeyCooldownDuration)
	}
	// 分流验证
	if dec := cfg.RouteRequest("gpt-4o-gemini"); dec.Line != LineGemini {
		t.Fatalf("gpt-4o-gemini 应走 gemini，得到 %+v", dec)
	}
	if dec := cfg.RouteRequest("gpt-4o"); dec.Line != LineOpenCode {
		t.Fatalf("gpt-4o 应走 opencode，得到 %+v", dec)
	}
}

func TestLoadConfig_NoGemini_Compatible(t *testing.T) {
	yamlContent := `
server:
  port: 8080
  api_keys: ["sk-1"]
default:
  fallback_model: "deepseek-v4-flash-free"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gemini != nil {
		t.Fatal("未配置 gemini 时应为 nil")
	}
	if cfg.Server.MaxBodyMB != 100 {
		t.Fatalf("max_body_mb 未配置时应默认 100，得到 %d", cfg.Server.MaxBodyMB)
	}
	if dec := cfg.RouteRequest("gpt-4o"); dec.Line != LineOpenCode {
		t.Fatalf("无 gemini 配置应恒 opencode，得到 %+v", dec)
	}
}

// max_body_mb 显式配置生效；负数/零回退默认
func TestLoadConfig_MaxBodyMB(t *testing.T) {
	write := func(t *testing.T, serverExtra string) *Config {
		t.Helper()
		yamlContent := `
server:
  port: 8080
` + serverExtra + `
default:
  fallback_model: "x"
`
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if cfg := write(t, "  max_body_mb: 64"); cfg.Server.MaxBodyMB != 64 {
		t.Fatalf("max_body_mb: 64 应生效，得到 %d", cfg.Server.MaxBodyMB)
	}
	if cfg := write(t, "  max_body_mb: -5"); cfg.Server.MaxBodyMB != 100 {
		t.Fatalf("负数 max_body_mb 应回退默认 100，得到 %d", cfg.Server.MaxBodyMB)
	}
}
