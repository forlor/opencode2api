package config

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "time/tzdata" // 内嵌时区库：daily_reset_tz 解析期校验依赖 LoadLocation（Windows 开发机无系统 tzdata）

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Host      string   `yaml:"host"`
		Port      int      `yaml:"port"`
		APIKeys   []string `yaml:"api_keys"`
		Secret    string   `yaml:"secret"`
		MaxBodyMB int      `yaml:"max_body_mb"`
	} `yaml:"server"`

	Default struct {
		CooldownDuration time.Duration     `yaml:"cooldown_duration"`
		FallbackModel    string            `yaml:"fallback_model"`
		ModelMappings    map[string]string `yaml:"model_mappings"`
	} `yaml:"default"`

	Nodes []NodeConfig `yaml:"nodes"`

	// Gemini 是可选的独立线路（Google AI Studio 号池）。为 nil/未启用时行为与旧配置完全一致。
	Gemini *GeminiConfig `yaml:"gemini"`
}

// NodeConfig 一个 OpenCode 节点的配置。现有字段保持兼容。
type NodeConfig struct {
	Name             string        `yaml:"name"`
	LANURL           string        `yaml:"lan_url"`
	SupportsIPChange bool          `yaml:"supports_ip_change"`
	IPChangeCommand  string        `yaml:"ip_change_command"`
	CooldownDuration time.Duration `yaml:"cooldown_duration"`
}

// 两条在线路标识（RouteDecision.Line 取值）
const (
	LineOpenCode = "opencode"
	LineGemini   = "gemini"
)

// RouteDecision 是一次客户端请求的分流决策结果
type RouteDecision struct {
	Line        string // LineOpenCode 或 LineGemini
	TargetModel string // 上游模型名（Gemini 线路为映射后的模型，尚未剥离后缀）
	Streaming   string // 仅 Gemini 线路使用：config 级 streaming_mode（"real"/"fake"/""）
}

// GeminiConfig 一条独立的 Google AI Studio 号池线路，与顶层 nodes 并行、互不影响
type GeminiConfig struct {
	Enabled                 bool          `yaml:"enabled"`
	Force                   bool          `yaml:"force"`                     // 调试用：所有请求强制走 Gemini（最高优先级）
	FallbackModel           string        `yaml:"fallback_model"`            // 未命中时的兜底 Gemini 模型
	DefaultMaxOutputTokens  int           `yaml:"default_max_output_tokens"` // Claude/Responses 缺 max_tokens 时兜底
	Models                  []string      `yaml:"models"`                    // 目标模型允许列表，支持 "gemini-*" 前缀通配
	ClientModels            []string      `yaml:"client_models"`             // 可选：客户端模型名直接点名走 Gemini
	MountPath               string        `yaml:"mount_path"`                // 节点 nginx 挂载前缀，默认 "/gemini"
	StreamingMode           string        `yaml:"streaming_mode"`            // "real"|"fake"|""（可由 -real/-fake 后缀覆盖）
	ForceThinking           bool          `yaml:"force_thinking"`
	ForceWebSearch          bool          `yaml:"force_web_search"`
	ForceCodeExecution      bool          `yaml:"force_code_execution"`
	ForceUrlContext         bool          `yaml:"force_url_context"`
	SafetySettingsThreshold string        `yaml:"safety_settings_threshold"` // 默认 "OFF"
	HTTPProxy               string        `yaml:"http_proxy"`                // 可选全局出站代理
	KeyCooldownDuration     time.Duration `yaml:"key_cooldown_duration"`     // 单 key 429 冷却，默认 60s
	BanAfterFailures        int32         `yaml:"ban_after_failures"`        // 连续失败阈值→禁用 key，默认 3
	MaxRPM                  int           `yaml:"max_rpm"`                   // 0=不限制
	MinKeyInterval          time.Duration `yaml:"min_key_interval"`          // 0=不限制

	KeyStrategy    string `yaml:"key_strategy"`     // round_robin（默认）| sequential（用完一个再换下一个）
	DailyLimit     int    `yaml:"daily_limit"`      // 每 key 每模型每日生成请求上限，0=不限制
	DailyResetTZ   string `yaml:"daily_reset_tz"`   // 每日配额重置时区，默认 America/Los_Angeles（Google 免费额度按 PT 午夜重置）
	DailyUsageFile string `yaml:"daily_usage_file"` // 每日用量持久化文件，默认 gemini_daily_usage.json
	UpstreamHost   string `yaml:"upstream_host"`    // 双 conf 部署时子节点 nginx gemini server 块的 server_name；空=不设 Host 头（行为同旧）

	Nodes []GeminiNodeConfig `yaml:"nodes"`
}

// GeminiNodeConfig 一个 Gemini 节点 = 一个 VPS（出口 IP）+ 一组绑定的 Gemini API Key
type GeminiNodeConfig struct {
	Name                string        `yaml:"name"`
	LANURL              string        `yaml:"lan_url"`
	APIKeys             []string      `yaml:"api_keys"`
	HTTPProxy           string        `yaml:"http_proxy"`            // 覆盖全局代理
	KeyCooldownDuration time.Duration `yaml:"key_cooldown_duration"` // 覆盖全局
	MaxRPM              int           `yaml:"max_rpm"`               // 覆盖全局
	MinKeyInterval      time.Duration `yaml:"min_key_interval"`      // 覆盖全局
	KeyStrategy         string        `yaml:"key_strategy"`          // 覆盖全局：round_robin|sequential
	DailyLimit          int           `yaml:"daily_limit"`           // 覆盖全局：0 表示继承全局
	UpstreamHost        string        `yaml:"upstream_host"`         // 覆盖全局：空表示继承全局
}

// ConfigYAML 用于 YAML 的反序列化辅助结构（支持解析字符串格式的时间，如 "30m"）
type ConfigYAML struct {
	Server struct {
		Host      string   `yaml:"host"`
		Port      int      `yaml:"port"`
		APIKeys   []string `yaml:"api_keys"`
		Secret    string   `yaml:"secret"`
		MaxBodyMB int      `yaml:"max_body_mb"`
	} `yaml:"server"`

	Default struct {
		CooldownDuration string            `yaml:"cooldown_duration"`
		FallbackModel    string            `yaml:"fallback_model"`
		ModelMapping     string            `yaml:"model_mapping"` // 向下兼容旧字段
		ModelMappings    map[string]string `yaml:"model_mappings"`
	} `yaml:"default"`

	Nodes []struct {
		Name             string `yaml:"name"`
		LANURL           string `yaml:"lan_url"`
		SupportsIPChange bool   `yaml:"supports_ip_change"`
		IPChangeCommand  string `yaml:"ip_change_command"`
		CooldownDuration string `yaml:"cooldown_duration"`
	} `yaml:"nodes"`

	Gemini *struct {
		Enabled                 bool     `yaml:"enabled"`
		Force                   bool     `yaml:"force"`
		FallbackModel           string   `yaml:"fallback_model"`
		DefaultMaxOutputTokens  int      `yaml:"default_max_output_tokens"`
		Models                  []string `yaml:"models"`
		ClientModels            []string `yaml:"client_models"`
		MountPath               string   `yaml:"mount_path"`
		StreamingMode           string   `yaml:"streaming_mode"`
		ForceThinking           bool     `yaml:"force_thinking"`
		ForceWebSearch          bool     `yaml:"force_web_search"`
		ForceCodeExecution      bool     `yaml:"force_code_execution"`
		ForceUrlContext         bool     `yaml:"force_url_context"`
		SafetySettingsThreshold string   `yaml:"safety_settings_threshold"`
		HTTPProxy               string   `yaml:"http_proxy"`
		KeyCooldownDuration     string   `yaml:"key_cooldown_duration"`
		BanAfterFailures        int32    `yaml:"ban_after_failures"`
		MaxRPM                  int      `yaml:"max_rpm"`
		MinKeyInterval          string   `yaml:"min_key_interval"`
		KeyStrategy             string   `yaml:"key_strategy"`
		DailyLimit              int      `yaml:"daily_limit"`
		DailyResetTZ            string   `yaml:"daily_reset_tz"`
		DailyUsageFile          string   `yaml:"daily_usage_file"`
		UpstreamHost            string   `yaml:"upstream_host"`
		Nodes                   []struct {
			Name                string   `yaml:"name"`
			LANURL              string   `yaml:"lan_url"`
			APIKeys             []string `yaml:"api_keys"`
			HTTPProxy           string   `yaml:"http_proxy"`
			KeyCooldownDuration string   `yaml:"key_cooldown_duration"`
			MaxRPM              int      `yaml:"max_rpm"`
			MinKeyInterval      string   `yaml:"min_key_interval"`
			KeyStrategy         string   `yaml:"key_strategy"`
			DailyLimit          int      `yaml:"daily_limit"`
			UpstreamHost        string   `yaml:"upstream_host"`
		} `yaml:"nodes"`
	} `yaml:"gemini"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw ConfigYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	cfg := &Config{}
	cfg.Server.Host = raw.Server.Host
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	cfg.Server.Port = raw.Server.Port
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	cfg.Server.APIKeys = raw.Server.APIKeys
	cfg.Server.Secret = raw.Server.Secret
	// 入站请求体上限（MB）：未配置或非法值时默认 100
	cfg.Server.MaxBodyMB = raw.Server.MaxBodyMB
	if cfg.Server.MaxBodyMB <= 0 {
		cfg.Server.MaxBodyMB = 100
	}

	// 解析默认冷却时间
	if raw.Default.CooldownDuration != "" {
		d, err := time.ParseDuration(raw.Default.CooldownDuration)
		if err == nil {
			cfg.Default.CooldownDuration = d
		}
	}
	if cfg.Default.CooldownDuration == 0 {
		cfg.Default.CooldownDuration = 30 * time.Minute
	}

	// 解析模型映射与兜底配置
	cfg.Default.FallbackModel = raw.Default.FallbackModel
	if cfg.Default.FallbackModel == "" {
		// 向下兼容旧版单模型配置
		if raw.Default.ModelMapping != "" {
			cfg.Default.FallbackModel = raw.Default.ModelMapping
		} else {
			cfg.Default.FallbackModel = "deepseek-v4-flash-free"
		}
	}

	if raw.Default.ModelMappings != nil {
		cfg.Default.ModelMappings = raw.Default.ModelMappings
	} else {
		cfg.Default.ModelMappings = make(map[string]string)
	}

	// 解析各个节点
	for _, rawNode := range raw.Nodes {
		node := NodeConfig{
			Name:             rawNode.Name,
			LANURL:           rawNode.LANURL,
			SupportsIPChange: rawNode.SupportsIPChange,
			IPChangeCommand:  rawNode.IPChangeCommand,
		}

		if rawNode.CooldownDuration != "" {
			d, err := time.ParseDuration(rawNode.CooldownDuration)
			if err == nil {
				node.CooldownDuration = d
			}
		}
		if node.CooldownDuration == 0 {
			node.CooldownDuration = cfg.Default.CooldownDuration
		}

		cfg.Nodes = append(cfg.Nodes, node)
	}

	// 解析可选的 Gemini 线路（为 nil 时与旧配置行为完全一致）
	if raw.Gemini != nil {
		g, err := parseGemini(raw.Gemini)
		if err != nil {
			return nil, err
		}
		cfg.Gemini = g
	}

	return cfg, nil
}

// parseDurationOr 解析字符串时长，失败或为空时返回默认值
func parseDurationOr(s string, def time.Duration) time.Duration {
	if s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}

// validUpstreamHost 校验 upstream_host：裸主机名（可选尾缀端口）。误带 scheme/路径/空白
// 会让子节点 nginx 的 server_name 匹配失败落入兜底 server，启动探测 404 即误 Ban 全部 key
// 且 24h 复探无法自愈——后果不可运行期自愈，故在解析期 fail fast。
func validUpstreamHost(v string) bool {
	host := v
	if i := strings.LastIndex(host, ":"); i >= 0 {
		port := host[i+1:]
		if port == "" {
			return false
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return false
			}
		}
		host = host[:i]
	}
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// parseGemini 将 YAML 镜像结构转换为 GeminiConfig，并填充默认值。
// upstream_host / daily_reset_tz 非法时返回 error（fail fast：两者错配的后果
// 分别是全量 key 误 Ban 与重置边界静默漂移数小时，均无法运行期自愈）。
func parseGemini(raw *struct {
	Enabled                 bool     `yaml:"enabled"`
	Force                   bool     `yaml:"force"`
	FallbackModel           string   `yaml:"fallback_model"`
	DefaultMaxOutputTokens  int      `yaml:"default_max_output_tokens"`
	Models                  []string `yaml:"models"`
	ClientModels            []string `yaml:"client_models"`
	MountPath               string   `yaml:"mount_path"`
	StreamingMode           string   `yaml:"streaming_mode"`
	ForceThinking           bool     `yaml:"force_thinking"`
	ForceWebSearch          bool     `yaml:"force_web_search"`
	ForceCodeExecution      bool     `yaml:"force_code_execution"`
	ForceUrlContext         bool     `yaml:"force_url_context"`
	SafetySettingsThreshold string   `yaml:"safety_settings_threshold"`
	HTTPProxy               string   `yaml:"http_proxy"`
	KeyCooldownDuration     string   `yaml:"key_cooldown_duration"`
	BanAfterFailures        int32    `yaml:"ban_after_failures"`
	MaxRPM                  int      `yaml:"max_rpm"`
	MinKeyInterval          string   `yaml:"min_key_interval"`
	KeyStrategy             string   `yaml:"key_strategy"`
	DailyLimit              int      `yaml:"daily_limit"`
	DailyResetTZ            string   `yaml:"daily_reset_tz"`
	DailyUsageFile          string   `yaml:"daily_usage_file"`
	UpstreamHost            string   `yaml:"upstream_host"`
	Nodes                   []struct {
		Name                string   `yaml:"name"`
		LANURL              string   `yaml:"lan_url"`
		APIKeys             []string `yaml:"api_keys"`
		HTTPProxy           string   `yaml:"http_proxy"`
		KeyCooldownDuration string   `yaml:"key_cooldown_duration"`
		MaxRPM              int      `yaml:"max_rpm"`
		MinKeyInterval      string   `yaml:"min_key_interval"`
		KeyStrategy         string   `yaml:"key_strategy"`
		DailyLimit          int      `yaml:"daily_limit"`
		UpstreamHost        string   `yaml:"upstream_host"`
	} `yaml:"nodes"`
}) (*GeminiConfig, error) {
	g := &GeminiConfig{
		Enabled:                 raw.Enabled,
		Force:                   raw.Force,
		FallbackModel:           raw.FallbackModel,
		Models:                  raw.Models,
		ClientModels:            raw.ClientModels,
		MountPath:               raw.MountPath,
		StreamingMode:           raw.StreamingMode,
		ForceThinking:           raw.ForceThinking,
		ForceWebSearch:          raw.ForceWebSearch,
		ForceCodeExecution:      raw.ForceCodeExecution,
		ForceUrlContext:         raw.ForceUrlContext,
		SafetySettingsThreshold: raw.SafetySettingsThreshold,
		HTTPProxy:               raw.HTTPProxy,
		BanAfterFailures:        raw.BanAfterFailures,
		MaxRPM:                  raw.MaxRPM,
	}

	if g.FallbackModel == "" {
		g.FallbackModel = "gemini-2.5-flash"
	}
	if g.DefaultMaxOutputTokens <= 0 {
		g.DefaultMaxOutputTokens = 8192
	}
	if g.MountPath == "" {
		g.MountPath = "/gemini"
	}
	if g.StreamingMode != "fake" {
		g.StreamingMode = "real" // 兜底：非法或为空一律 real
	}
	if g.SafetySettingsThreshold == "" {
		g.SafetySettingsThreshold = "OFF"
	}
	if g.BanAfterFailures <= 0 {
		g.BanAfterFailures = 3
	}
	g.KeyCooldownDuration = parseDurationOr(raw.KeyCooldownDuration, 60*time.Second)
	g.MinKeyInterval = parseDurationOr(raw.MinKeyInterval, 0)

	// key 选择策略：空或非法一律 round_robin（兼容旧行为）
	g.KeyStrategy = raw.KeyStrategy
	if g.KeyStrategy != "sequential" {
		if g.KeyStrategy != "" && g.KeyStrategy != "round_robin" {
			log.Printf("[gemini] key_strategy %q 非法（仅支持 round_robin|sequential），回退 round_robin", raw.KeyStrategy)
		}
		g.KeyStrategy = "round_robin"
	}
	g.DailyLimit = raw.DailyLimit
	g.DailyResetTZ = raw.DailyResetTZ
	if g.DailyResetTZ == "" {
		g.DailyResetTZ = "America/Los_Angeles" // Google 免费额度按太平洋时间午夜重置
	} else if _, err := time.LoadLocation(g.DailyResetTZ); err != nil {
		// LoadLocation 大小写敏感（"asia/shanghai" 即失败），只回退 UTC 会把重置边界
		// 静默漂移数小时，且运行期仅启动日志一行——解析期直接报错
		return nil, fmt.Errorf("gemini.daily_reset_tz %q 非法（应为 IANA 时区名，注意大小写，如 Asia/Shanghai）: %w", g.DailyResetTZ, err)
	}
	g.DailyUsageFile = raw.DailyUsageFile
	if g.DailyUsageFile == "" {
		g.DailyUsageFile = "gemini_daily_usage.json"
	}
	g.UpstreamHost = raw.UpstreamHost
	if g.UpstreamHost != "" && !validUpstreamHost(g.UpstreamHost) {
		return nil, fmt.Errorf("gemini.upstream_host %q 非法（应为裸主机名，可带端口；不能含 scheme 或路径）", g.UpstreamHost)
	}

	for _, rawNode := range raw.Nodes {
		node := GeminiNodeConfig{
			Name:      rawNode.Name,
			LANURL:    rawNode.LANURL,
			APIKeys:   rawNode.APIKeys,
			HTTPProxy: rawNode.HTTPProxy,
			MaxRPM:    rawNode.MaxRPM,
		}
		if node.MaxRPM == 0 {
			node.MaxRPM = g.MaxRPM
		}
		node.KeyCooldownDuration = parseDurationOr(rawNode.KeyCooldownDuration, g.KeyCooldownDuration)
		node.MinKeyInterval = parseDurationOr(rawNode.MinKeyInterval, g.MinKeyInterval)
		// 节点级覆盖：空/0 表示继承全局
		node.KeyStrategy = rawNode.KeyStrategy
		if node.KeyStrategy == "" {
			node.KeyStrategy = g.KeyStrategy
		} else if node.KeyStrategy != "round_robin" && node.KeyStrategy != "sequential" {
			log.Printf("[gemini] 节点 %s key_strategy %q 非法，回退全局 %q", node.Name, rawNode.KeyStrategy, g.KeyStrategy)
			node.KeyStrategy = g.KeyStrategy
		}
		node.DailyLimit = rawNode.DailyLimit
		if node.DailyLimit <= 0 {
			node.DailyLimit = g.DailyLimit
		}
		node.UpstreamHost = rawNode.UpstreamHost
		if node.UpstreamHost == "" {
			node.UpstreamHost = g.UpstreamHost
		} else if !validUpstreamHost(node.UpstreamHost) {
			return nil, fmt.Errorf("gemini.nodes[%s].upstream_host %q 非法（应为裸主机名，可带端口；不能含 scheme 或路径）", node.Name, node.UpstreamHost)
		}
		g.Nodes = append(g.Nodes, node)
	}

	return g, nil
}

// GetMappedModel 根据客户端发来的模型名称，动态查询对应映射的目标模型；若未匹配则使用 fallback_model 兜底
func (c *Config) GetMappedModel(clientModel string) string {
	if mapped, ok := c.Default.ModelMappings[clientModel]; ok && mapped != "" {
		return mapped
	}
	if c.Default.FallbackModel != "" {
		return c.Default.FallbackModel
	}
	return "deepseek-v4-flash-free"
}

// RouteRequest 决定一次客户端请求走哪条线路。纯函数、无副作用，便于单测。
// 规则（优先级从高到低）：
//  1. Gemini 未配置/未启用 → 恒 opencode（旧配置零行为变化）
//  2. Gemini.Force → 恒 gemini（模型取 GeminiModelFor）
//  3. 常规：clientModel 命中 ClientModels/Models → gemini（用 clientModel）
//     否则 model_mappings 的目标命中 Models → gemini（用映射目标）
//     否则 → opencode
func (c *Config) RouteRequest(clientModel string) RouteDecision {
	if c.Gemini == nil || !c.Gemini.Enabled {
		return RouteDecision{Line: LineOpenCode, TargetModel: c.GetMappedModel(clientModel)}
	}

	mapped := ""
	if v, ok := c.Default.ModelMappings[clientModel]; ok {
		mapped = v
	}

	streaming := c.Gemini.StreamingMode

	if c.Gemini.Force {
		model := c.geminiModelFor(clientModel, mapped)
		return RouteDecision{Line: LineGemini, TargetModel: model, Streaming: streaming}
	}

	// 常规模式
	if matchClientModel(clientModel, c.Gemini.ClientModels) || MatchModel(clientModel, c.Gemini.Models) {
		return RouteDecision{Line: LineGemini, TargetModel: clientModel, Streaming: streaming}
	}
	if mapped != "" && MatchModel(mapped, c.Gemini.Models) {
		return RouteDecision{Line: LineGemini, TargetModel: mapped, Streaming: streaming}
	}
	return RouteDecision{Line: LineOpenCode, TargetModel: c.GetMappedModel(clientModel)}
}

// GeminiModelFor 计算 Force 模式下 Gemini 线路应使用的上游模型（尚未剥离后缀）
func (c *Config) GeminiModelFor(clientModel string) string {
	if c.Gemini == nil {
		return c.GeminiFallbackModel()
	}
	mapped := ""
	if v, ok := c.Default.ModelMappings[clientModel]; ok {
		mapped = v
	}
	return c.geminiModelFor(clientModel, mapped)
}

func (c *Config) geminiModelFor(clientModel, mapped string) string {
	if mapped != "" && MatchModel(mapped, c.Gemini.Models) {
		return mapped
	}
	if matchClientModel(clientModel, c.Gemini.ClientModels) || MatchModel(clientModel, c.Gemini.Models) {
		return clientModel
	}
	return c.GeminiFallbackModel()
}

// GeminiFallbackModel 返回 Gemini 线路的兜底模型（配置缺失时给默认值）
func (c *Config) GeminiFallbackModel() string {
	if c.Gemini != nil && c.Gemini.FallbackModel != "" {
		return c.Gemini.FallbackModel
	}
	return "gemini-2.5-flash"
}

// matchClientModel 客户端模型名精确命中列表（client_models 语义）
func matchClientModel(model string, list []string) bool {
	for _, m := range list {
		if m != "" && m == model {
			return true
		}
	}
	return false
}

// MatchModel 判断模型名是否命中模式列表。仅支持尾缀 "*" 前缀通配与精确匹配。
func MatchModel(model string, patterns []string) bool {
	if model == "" {
		return false
	}
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if p == model {
			return true
		}
	}
	return false
}
