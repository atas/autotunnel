package config

import (
	_ "embed"
	"os"
	"path/filepath"
)

// defaultConfigTemplate is the template for a new config file
//
//go:embed default_config.yaml
var defaultConfigTemplate string

// DefaultConfig returns configuration with sensible defaults for optional fields
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		HTTP: HTTPConfig{
			K8s: K8sConfig{
				Kubeconfig: filepath.Join(home, ".kube", "config"),
				Routes:     make(map[string]K8sRouteConfig),
			},
		},
	}
}

// CreateDefaultConfig creates a new config file with example content
func CreateDefaultConfig(path string) error {
	return os.WriteFile(path, []byte(defaultConfigTemplate), 0644)
}
