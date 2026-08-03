package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port    int      `yaml:"port"`
		APIKeys []string `yaml:"api_keys"`
		Secret  string   `yaml:"secret"`
	} `yaml:"server"`

	Default struct {
		CooldownDuration time.Duration     `yaml:"cooldown_duration"`
		FallbackModel    string            `yaml:"fallback_model"`
		ModelMappings    map[string]string `yaml:"model_mappings"`
	} `yaml:"default"`

	Nodes []NodeConfig `yaml:"nodes"`
}

type NodeConfig struct {
	Name             string        `yaml:"name"`
	LANURL           string        `yaml:"lan_url"`
	SupportsIPChange bool          `yaml:"supports_ip_change"`
	IPChangeCommand  string        `yaml:"ip_change_command"`
	CooldownDuration time.Duration `yaml:"cooldown_duration"`
}

// ConfigYAML 用于 YAML 的反序列化辅助结构（支持解析字符串格式的时间，如 "30m"）
type ConfigYAML struct {
	Server struct {
		Port    int      `yaml:"port"`
		APIKeys []string `yaml:"api_keys"`
		Secret  string   `yaml:"secret"`
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
	cfg.Server.Port = raw.Server.Port
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	cfg.Server.APIKeys = raw.Server.APIKeys
	cfg.Server.Secret = raw.Server.Secret

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

	return cfg, nil
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
