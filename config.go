package main

import (
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	pluginName       = "ex-plugin"
	defaultResponses = "https://bps.openai.com/basispoints/api/responses"
	defaultAuthMode  = "chatgpt"
)

// pluginVersion is overwritten by release builds with -X main.pluginVersion.
var pluginVersion = "0.1.1"

type pluginConfig struct {
	Enabled               *bool  `yaml:"enabled"`
	ResponsesURL          string `yaml:"responses_url"`
	AuthMode              string `yaml:"auth_mode"`
	ToolsVersionID        string `yaml:"tools_version_id"`
	ForwardPromptCacheKey *bool  `yaml:"forward_prompt_cache_key"`
	CatalogAtPromptEnd    *bool  `yaml:"catalog_at_prompt_end"`
}

func (c pluginConfig) enabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c pluginConfig) responsesURL() string {
	if strings.TrimSpace(c.ResponsesURL) == "" {
		return defaultResponses
	}
	return strings.TrimSpace(c.ResponsesURL)
}

func (c pluginConfig) authMode() string {
	if strings.TrimSpace(c.AuthMode) == "" {
		return defaultAuthMode
	}
	return strings.TrimSpace(c.AuthMode)
}

func (c pluginConfig) forwardPromptCacheKey() bool {
	return c.ForwardPromptCacheKey == nil || *c.ForwardPromptCacheKey
}

func (c pluginConfig) catalogAtPromptEnd() bool {
	return c.CatalogAtPromptEnd != nil && *c.CatalogAtPromptEnd
}

var currentConfig atomic.Value

func init() {
	currentConfig.Store(pluginConfig{})
}

func loadedConfig() pluginConfig {
	value := currentConfig.Load()
	if value == nil {
		return pluginConfig{}
	}
	cfg, _ := value.(pluginConfig)
	return cfg
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := decodeJSONUseNumber(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	currentConfig.Store(cfg)
	return nil
}
