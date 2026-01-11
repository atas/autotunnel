package config

import "time"

// HTTPConfig represents HTTP protocol configuration
type HTTPConfig struct {
	ListenAddr  string        `yaml:"listen"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	K8s         K8sConfig     `yaml:"k8s"`
}

// K8sConfig represents Kubernetes backend configuration
type K8sConfig struct {
	Kubeconfig          string                    `yaml:"kubeconfig"`
	ResolvedKubeconfigs []string                  `yaml:"-"` // Computed: resolved paths (not from YAML)
	Routes              map[string]K8sRouteConfig `yaml:"routes"`
}

// K8sRouteConfig represents a Kubernetes route mapping (service or pod)
type K8sRouteConfig struct {
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
	Service   string `yaml:"service"` // Target service name (mutually exclusive with Pod)
	Pod       string `yaml:"pod"`     // Target pod name directly (mutually exclusive with Service)
	Port      int    `yaml:"port"`
	Scheme    string `yaml:"scheme"` // "http" or "https" - controls X-Forwarded-Proto header (default: http)
}
