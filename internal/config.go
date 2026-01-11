package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

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

// HTTPConfig represents HTTP protocol configuration
type HTTPConfig struct {
	ListenAddr  string        `yaml:"listen"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	K8s         K8sConfig     `yaml:"k8s"`
}

// K8sConfig represents Kubernetes backend configuration
type K8sConfig struct {
	Kubeconfig string                    `yaml:"kubeconfig"`
	Routes     map[string]K8sRouteConfig `yaml:"routes"`
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

// DefaultConfig returns configuration with sensible defaults
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Kubeconfig: filepath.Join(home, ".kube", "config"),
				Routes:     make(map[string]K8sRouteConfig),
			},
		},
	}
}

// LoadConfig loads configuration from a YAML file
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

	return cfg, nil
}

// DefaultConfigTemplate is the template for a new config file
const DefaultConfigTemplate = `apiVersion: lazyfwd/v1

# Verbose logging (can also use --verbose flag with higher priority)
# verbose: false

# Auto-reload on file changes. Any changes need ` + "`brew services restart lazyfwd`" + ` while it is false.
auto_reload_config: true

http:
  # Listen address - handles both HTTP and HTTPS (TLS passthrough) on same port
  listen: ":8989"  # Port changes require: brew services restart lazyfwd

  # Idle timeout before closing tunnels (Go duration format)
  # After this duration of no traffic, the tunnel will be closed
  idle_timeout: 60m

  k8s:
    # Path to kubeconfig (optional, defaults to ~/.kube/config)
    # kubeconfig: ~/.kube/config

    routes:
      # # https://argocd.localhost:8989 (also supports http http://argocd.localhost:8989)
      # argocd.localhost:
      #   context: my-cluster-context # Kubernetes context name from kubeconfig
      #   namespace: argocd           # Kubernetes namespace
      #   service: argocd-server      # Kubernetes service name
      #   port: 443                   # Service port (automatically resolves to container targetPort)
      #   scheme: https               # Set X-Forwarded-Proto header (use "https" to prevent HTTPS redirects)

      # # http://grafana.localhost:8989
      # grafana.localhost:
      #   context: my-cluster-context
      #   namespace: monitoring
      #   service: grafana
      #   port: 3000
      #   scheme: http               # Default is "http", no need to specify

      # http://debug.localhost:8989
      # debug.localhost:
      #   context: microk8s
      #   namespace: default
      #   pod: my-debug-pod         # Pod name (use instead of service)
      #   port: 8080
`

// CreateDefaultConfig creates a new config file with example content
func CreateDefaultConfig(path string) error {
	return os.WriteFile(path, []byte(DefaultConfigTemplate), 0644)
}

// ConfigExists checks if the config file exists
func ConfigExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ShouldAutoReload returns whether config auto-reload is enabled (default true)
func (c *Config) ShouldAutoReload() bool {
	if c.AutoReloadConfig == nil {
		return true
	}
	return *c.AutoReloadConfig
}

// LogRoutes prints the configured routes in a formatted table
func (c *Config) LogRoutes() {
	fmt.Printf("Routes (%d):\n", len(c.HTTP.K8s.Routes))

	type routeInfo struct {
		hostname string
		local    string
		target   string
		context  string
	}
	var routes []routeInfo
	maxLocal, maxTarget := 0, 0
	for hostname, route := range c.HTTP.K8s.Routes {
		scheme := route.Scheme
		if scheme == "" {
			scheme = "http"
		}
		local := fmt.Sprintf("%s://%s%s", scheme, hostname, c.HTTP.ListenAddr)
		var target string
		if route.Pod != "" {
			target = fmt.Sprintf("pod/%s:%d", route.Pod, route.Port)
		} else {
			target = fmt.Sprintf("%s:%d", route.Service, route.Port)
		}
		context := fmt.Sprintf("%s/%s", route.Context, route.Namespace)
		routes = append(routes, routeInfo{hostname, local, target, context})
		if len(local) > maxLocal {
			maxLocal = len(local)
		}
		if len(target) > maxTarget {
			maxTarget = len(target)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].hostname < routes[j].hostname
	})
	for _, r := range routes {
		fmt.Printf("  %-*s  ->  %-*s  (%s)\n", maxLocal, r.local, maxTarget, r.target, r.context)
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	// Check API version compatibility
	if c.ApiVersion == "" {
		c.ApiVersion = CurrentApiVersion // Default for old configs without apiVersion
	}
	if c.ApiVersion != CurrentApiVersion {
		return fmt.Errorf("unsupported config apiVersion %q (this version of lazyfwd requires %q)", c.ApiVersion, CurrentApiVersion)
	}

	if c.HTTP.ListenAddr == "" {
		return fmt.Errorf("http.listen is required")
	}

	if c.HTTP.IdleTimeout <= 0 {
		return fmt.Errorf("http.idle_timeout must be positive")
	}

	for hostname, route := range c.HTTP.K8s.Routes {
		if route.Context == "" {
			return fmt.Errorf("route %q: context is required", hostname)
		}
		if route.Namespace == "" {
			return fmt.Errorf("route %q: namespace is required", hostname)
		}
		// Require exactly one of service or pod
		if route.Service == "" && route.Pod == "" {
			return fmt.Errorf("route %q: either service or pod is required", hostname)
		}
		if route.Service != "" && route.Pod != "" {
			return fmt.Errorf("route %q: cannot specify both service and pod", hostname)
		}
		if route.Port <= 0 || route.Port > 65535 {
			return fmt.Errorf("route %q: port must be between 1 and 65535", hostname)
		}
	}

	return nil
}
