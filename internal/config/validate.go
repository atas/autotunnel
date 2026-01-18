package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// hostnameRegex matches valid DNS hostnames (RFC 1123)
var hostnameRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-\.]*[a-zA-Z0-9])?$`)

// imageNameRegex matches valid container image names
var imageNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/:@]*$`)

// IsValidTargetHost checks if a host string is safe for use in shell commands.
// Allows valid hostnames (RFC 1123) and IP addresses only.
// Prevents command injection via malicious host values in socat routes.
func IsValidTargetHost(host string) bool {
	if host == "" {
		return false
	}

	// Valid IP address (v4 or v6)
	if ip := net.ParseIP(host); ip != nil {
		return true
	}

	// Valid hostname: max 253 chars total
	if len(host) > 253 {
		return false
	}

	return hostnameRegex.MatchString(host)
}

// IsValidImageName checks if an image name is safe for use.
// Rejects shell metacharacters to prevent command injection.
func IsValidImageName(image string) bool {
	if image == "" || len(image) > 256 {
		return false
	}
	// Reject shell metacharacters
	for _, c := range image {
		if c == ';' || c == '|' || c == '&' || c == '$' || c == '`' ||
			c == '(' || c == ')' || c == '\'' || c == '"' || c == ' ' ||
			c == '\n' || c == '\t' {
			return false
		}
	}
	return imageNameRegex.MatchString(image)
}

// validateLocalPort validates TCP local port: range, HTTP conflict, and duplicates
func validateLocalPort(routeID string, localPort, httpPort int, seenPorts map[int]string, source string) error {
	if localPort <= 0 || localPort > 65535 {
		return fmt.Errorf("%s: local port must be between 1 and 65535", routeID)
	}
	if localPort == httpPort {
		return fmt.Errorf("%s: conflicts with http.listen port", routeID)
	}
	if existing, exists := seenPorts[localPort]; exists {
		return fmt.Errorf("%s: port already used in tcp.k8s.%s", routeID, existing)
	}
	seenPorts[localPort] = source
	return nil
}

// validateRouteBase validates the common fields shared by HTTP and TCP routes
func validateRouteBase(routeID, context, namespace, service, pod string, port int) error {
	if context == "" {
		return fmt.Errorf("%s: context is required", routeID)
	}
	if namespace == "" {
		return fmt.Errorf("%s: namespace is required", routeID)
	}
	if service == "" && pod == "" {
		return fmt.Errorf("%s: either service or pod is required", routeID)
	}
	if service != "" && pod != "" {
		return fmt.Errorf("%s: cannot specify both service and pod", routeID)
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s: port must be between 1 and 65535", routeID)
	}
	return nil
}

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
		routeID := fmt.Sprintf("route %q", hostname)
		if err := validateRouteBase(routeID, route.Context, route.Namespace, route.Service, route.Pod, route.Port); err != nil {
			return err
		}
	}

	// Validate TCP config (optional - skip if no routes configured)
	if err := c.validateTCP(); err != nil {
		return err
	}

	return nil
}

func (c *Config) validateTCP() error {
	hasRoutes := len(c.TCP.K8s.Routes) > 0
	hasJump := len(c.TCP.K8s.Jump) > 0

	if !hasRoutes && !hasJump {
		return nil
	}

	// Check idle timeout if set
	if c.TCP.IdleTimeout < 0 {
		return fmt.Errorf("tcp.idle_timeout cannot be negative")
	}

	// Extract HTTP listen port for conflict checking
	httpPort, err := extractPort(c.HTTP.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid http.listen address: %w", err)
	}

	// Track all seen TCP ports (both routes and jump)
	seenPorts := make(map[int]string) // port -> source ("routes" or "jump")

	// Validate direct port-forward routes
	for localPort, route := range c.TCP.K8s.Routes {
		routeID := fmt.Sprintf("tcp.k8s.routes[%d]", localPort)

		if err := validateLocalPort(routeID, localPort, httpPort, seenPorts, "routes"); err != nil {
			return err
		}
		if err := validateRouteBase(routeID, route.Context, route.Namespace, route.Service, route.Pod, route.Port); err != nil {
			return err
		}
	}

	// Validate jump (jump-host) routes
	for localPort, route := range c.TCP.K8s.Jump {
		routeID := fmt.Sprintf("tcp.k8s.jump[%d]", localPort)

		if err := validateLocalPort(routeID, localPort, httpPort, seenPorts, "jump"); err != nil {
			return err
		}

		if route.Context == "" {
			return fmt.Errorf("%s: context is required", routeID)
		}
		if route.Namespace == "" {
			return fmt.Errorf("%s: namespace is required", routeID)
		}

		// Validate via (jump pod) - require exactly one of pod or service
		if route.Via.Pod == "" && route.Via.Service == "" {
			return fmt.Errorf("%s: via.pod or via.service is required", routeID)
		}
		if route.Via.Pod != "" && route.Via.Service != "" {
			return fmt.Errorf("%s: cannot specify both via.pod and via.service", routeID)
		}

		// Validate create config
		if route.Via.Create != nil {
			if route.Via.Service != "" {
				return fmt.Errorf("%s: via.create cannot be used with via.service", routeID)
			}
			if route.Via.Pod == "" {
				return fmt.Errorf("%s: via.pod is required when using via.create", routeID)
			}
			if route.Via.Create.Image == "" {
				return fmt.Errorf("%s: via.create.image is required", routeID)
			}
			if !IsValidImageName(route.Via.Create.Image) {
				return fmt.Errorf("%s: via.create.image %q is invalid", routeID, route.Via.Create.Image)
			}
		}

		// Validate target host - must be valid hostname/IP to prevent command injection
		if route.Target.Host == "" {
			return fmt.Errorf("%s: target.host is required", routeID)
		}
		if !IsValidTargetHost(route.Target.Host) {
			return fmt.Errorf("%s: target.host %q contains invalid characters (must be valid hostname or IP)", routeID, route.Target.Host)
		}

		// Validate target port
		if route.Target.Port <= 0 || route.Target.Port > 65535 {
			return fmt.Errorf("%s: target.port must be between 1 and 65535", routeID)
		}

		// Validate method (allow empty or "socat" for now)
		if route.Method != "" && route.Method != "socat" {
			return fmt.Errorf("%s: unsupported method %q (only \"socat\" is currently supported)", routeID, route.Method)
		}
	}

	return nil
}

// extractPort extracts the port number from an address string like ":8989" or "127.0.0.1:8989"
func extractPort(addr string) (int, error) {
	idx := strings.LastIndex(addr, ":")
	if idx == -1 {
		return 0, fmt.Errorf("no port in address %q", addr)
	}
	portStr := addr[idx+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q in address %q", portStr, addr)
	}
	return port, nil
}
