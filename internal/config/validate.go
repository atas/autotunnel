package config

import "fmt"

// Validate checks if the configuration is valid.
// Returns nil if valid, otherwise returns an error describing the problem.
func (c *Config) Validate() error {
	// Allow empty apiVersion (defaults to current), but reject wrong versions
	if c.ApiVersion != "" && c.ApiVersion != CurrentApiVersion {
		return fmt.Errorf("unsupported config apiVersion %q (this version of autotunnel requires %q)", c.ApiVersion, CurrentApiVersion)
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
