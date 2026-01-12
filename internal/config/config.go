// Package config handles YAML configuration loading and validation for autotunnel.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CurrentApiVersion is the config format version supported by this build
const CurrentApiVersion = "autotunnel/v1"

// Config represents the application configuration
type Config struct {
	ApiVersion       string     `yaml:"apiVersion"`
	Verbose          bool       `yaml:"verbose"`
	AutoReloadConfig *bool      `yaml:"auto_reload_config"` // nil = true (default)
	ExecPath         []string   `yaml:"exec_path"`          // Additional PATH entries for exec credential plugins
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

	// Resolve kubeconfig paths (supports multiple colon-separated paths and $KUBECONFIG fallback)
	cfg.HTTP.K8s.ResolvedKubeconfigs = resolveKubeconfigs(cfg.HTTP.K8s.Kubeconfig)

	// Validate the loaded configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// resolveKubeconfigs resolves kubeconfig path(s) with the following priority:
// 1. Explicit path(s) from config (colon-separated on Unix, semicolon on Windows)
// 2. $KUBECONFIG environment variable
// 3. Default ~/.kube/config
func resolveKubeconfigs(configValue string) []string {
	var pathStr string

	if configValue != "" {
		// Use explicit paths from config
		pathStr = configValue
	} else if envValue := os.Getenv("KUBECONFIG"); envValue != "" {
		// Fall back to $KUBECONFIG environment variable
		pathStr = envValue
	} else {
		// Fall back to default ~/.kube/config
		home, _ := os.UserHomeDir()
		return []string{filepath.Join(home, ".kube", "config")}
	}

	// Split by OS-specific path separator (: on Unix, ; on Windows)
	paths := filepath.SplitList(pathStr)

	// Expand tilde in each path
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
