package config

import (
	_ "embed"
	"os"
)

//go:embed default_config.yaml
var defaultConfigTemplate string

func DefaultConfig() *Config {
	return &Config{
		HTTP: HTTPConfig{
			K8s: K8sConfig{
				Kubeconfig: "", // Empty = try $KUBECONFIG env var, then ~/.kube/config
				Routes:     make(map[string]K8sRouteConfig),
			},
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Routes: make(map[int]TCPRouteConfig),
			},
		},
	}
}

func CreateDefaultConfig(path string) error {
	return os.WriteFile(path, []byte(defaultConfigTemplate), 0644)
}
