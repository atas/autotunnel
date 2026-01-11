// Package config handles YAML configuration loading and validation for lazyfwd.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CurrentApiVersion is the config format version supported by this build
const CurrentApiVersion = "lazyfwd/v1"

// Config represents the application configuration
type Config struct {
	ApiVersion       string     `yaml:"apiVersion"`
	Verbose          bool       `yaml:"verbose"`
	AutoReloadConfig *bool      `yaml:"auto_reload_config"` // nil = true (default)
	HTTP             HTTPConfig `yaml:"http"`
}

// LoadConfig loads configuration from a YAML file and validates it.
// This function calls Validate() internally - callers do not need to validate separately.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Expand home directory in kubeconfig path
	if len(cfg.HTTP.K8s.Kubeconfig) > 0 && cfg.HTTP.K8s.Kubeconfig[0] == '~' {
		home, _ := os.UserHomeDir()
		cfg.HTTP.K8s.Kubeconfig = filepath.Join(home, cfg.HTTP.K8s.Kubeconfig[1:])
	}

	// Validate the loaded configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
