package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const CurrentApiVersion = "autotunnel/v1"

type Config struct {
	ApiVersion       string     `yaml:"apiVersion"`
	Verbose          bool       `yaml:"verbose"`
	AutoReloadConfig *bool      `yaml:"auto_reload_config"` // nil = true (default)
	ExecPath         []string   `yaml:"exec_path"`          // Additional PATH entries for exec credential plugins
	HTTP             HTTPConfig `yaml:"http"`
	TCP              TCPConfig  `yaml:"tcp"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	cfg.HTTP.K8s.ResolvedKubeconfigs = resolveKubeconfigs(cfg.HTTP.K8s.Kubeconfig)

	if cfg.TCP.K8s.Kubeconfig != "" {
		cfg.TCP.K8s.ResolvedKubeconfigs = resolveKubeconfigs(cfg.TCP.K8s.Kubeconfig)
	} else {
		cfg.TCP.K8s.ResolvedKubeconfigs = cfg.HTTP.K8s.ResolvedKubeconfigs
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// resolveKubeconfigs supports: explicit paths, $KUBECONFIG, or default ~/.kube/config
func resolveKubeconfigs(configValue string) []string {
	var pathStr string

	if configValue != "" {
		pathStr = configValue
	} else if envValue := os.Getenv("KUBECONFIG"); envValue != "" {
		pathStr = envValue
	} else {
		home, _ := os.UserHomeDir()
		return []string{filepath.Join(home, ".kube", "config")}
	}

	paths := filepath.SplitList(pathStr)

	home, _ := os.UserHomeDir()
	resolved := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		} else if p == "~" {
			p = home
		}
		resolved = append(resolved, p)
	}

	return resolved
}
