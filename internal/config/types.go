package config

import "time"

type HTTPConfig struct {
	ListenAddr  string        `yaml:"listen"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	K8s         K8sConfig     `yaml:"k8s"`
}

type K8sConfig struct {
	Kubeconfig          string                    `yaml:"kubeconfig"`
	ResolvedKubeconfigs []string                  `yaml:"-"` // computed at load time
	Routes              map[string]K8sRouteConfig `yaml:"routes"`
	DynamicHost         string                    `yaml:"dynamic_host"`
}

type K8sRouteConfig struct {
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
	Service   string `yaml:"service"` // Target service name (mutually exclusive with Pod)
	Pod       string `yaml:"pod"`     // Target pod name directly (mutually exclusive with Service)
	Port      int    `yaml:"port"`
	Scheme    string `yaml:"scheme"` // "http" or "https" - controls X-Forwarded-Proto header (default: http)
}

type TCPConfig struct {
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	K8s         TCPK8sConfig  `yaml:"k8s"`
}

type TCPK8sConfig struct {
	Kubeconfig          string                   `yaml:"kubeconfig"`
	ResolvedKubeconfigs []string                 `yaml:"-"` // Computed: resolved paths (not from YAML)
	Routes              map[int]TCPRouteConfig   `yaml:"routes"` // local port -> direct port-forward route
	Jump                map[int]JumpRouteConfig  `yaml:"jump"`   // local port -> jump-host route via exec+socat/nc
}

// TCPRouteConfig defines a single TCP route (simpler than K8sRouteConfig - no Scheme field)
type TCPRouteConfig struct {
	Context   string `yaml:"context"`
	Namespace string `yaml:"namespace"`
	Service   string `yaml:"service"` // Target service name (mutually exclusive with Pod)
	Pod       string `yaml:"pod"`     // Target pod name directly (mutually exclusive with Service)
	Port      int    `yaml:"port"`    // Target port on the service/pod
}


// JumpRouteConfig defines a jump-host route via kubectl exec + socat/nc
// This allows connecting to VPC-internal services (RDS, Cloud SQL, etc.) through a jump pod
type JumpRouteConfig struct {
	Context   string       `yaml:"context"`         // K8s context name
	Namespace string       `yaml:"namespace"`       // K8s namespace
	Via       ViaConfig    `yaml:"via"`             // Jump pod configuration
	Target    TargetConfig `yaml:"target"`          // External target (e.g., RDS hostname)
	Method    string       `yaml:"method,omitempty"` // "socat" (default) or future alternatives
}

// GetMethod returns the forwarding method, defaulting to "socat" if not specified
func (r *JumpRouteConfig) GetMethod() string {
	if r.Method == "" {
		return "socat"
	}
	return r.Method
}

type ViaConfig struct {
	Pod       string `yaml:"pod,omitempty"`       // Direct pod name (mutually exclusive with Service)
	Service   string `yaml:"service,omitempty"`   // Service to discover pod from (mutually exclusive with Pod)
	Container string `yaml:"container,omitempty"` // Container name (optional, for multi-container pods)
}

type TargetConfig struct {
	Host string `yaml:"host"` // Target hostname (e.g., mydb.cluster-xyz.us-east-1.rds.amazonaws.com)
	Port int    `yaml:"port"` // Target port
}
