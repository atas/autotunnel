package tunnelmgr

import (
	"strconv"
	"strings"

	"github.com/atas/lazyfwd/internal/config"
)

// ParseDynamicHostname parses a dynamic hostname into a K8sRouteConfig.
// Supports both services and pods:
//   - Service: {service}-{port}.svc.{namespace}.ns.{context}.cx.{dynamicHost}
//   - Pod:     {pod}-{port}.pod.{namespace}.ns.{context}.cx.{dynamicHost}
//
// Examples:
//   - argocd-server-443.svc.argocd.ns.my-cluster.cx.k8s.localhost
//   - nginx-2fxac-80.pod.default.ns.microk8s.cx.k8s.localhost
//
// Returns the parsed config and true if valid, nil and false otherwise.
func ParseDynamicHostname(hostname, dynamicHost, scheme string) (*config.K8sRouteConfig, bool) {
	if dynamicHost == "" {
		return nil, false
	}

	// Build expected suffix: ".cx.k8s.localhost"
	suffix := ".cx." + dynamicHost
	if !strings.HasSuffix(hostname, suffix) {
		return nil, false
	}

	// Strip suffix → "argocd-server-443.svc.argocd.ns.my-cluster"
	core := strings.TrimSuffix(hostname, suffix)

	// Find ".ns." to extract context (everything after .ns.)
	nsIdx := strings.LastIndex(core, ".ns.")
	if nsIdx == -1 {
		return nil, false
	}
	context := core[nsIdx+4:] // Skip ".ns."
	core = core[:nsIdx]       // "argocd-server-443.svc.argocd"

	// Try to find ".svc." or ".pod." marker
	var namespace, targetPort string
	var isService bool

	if svcIdx := strings.LastIndex(core, ".svc."); svcIdx != -1 {
		namespace = core[svcIdx+5:] // Skip ".svc."
		targetPort = core[:svcIdx]  // "argocd-server-443"
		isService = true
	} else if podIdx := strings.LastIndex(core, ".pod."); podIdx != -1 {
		namespace = core[podIdx+5:] // Skip ".pod."
		targetPort = core[:podIdx]  // "nginx-2fxac-80"
		isService = false
	} else {
		return nil, false
	}

	// Validate extracted parts are not empty
	if context == "" || namespace == "" || targetPort == "" {
		return nil, false
	}

	// Split target-port by last "-" to get name and port
	lastDash := strings.LastIndex(targetPort, "-")
	if lastDash == -1 {
		return nil, false
	}

	name := targetPort[:lastDash]
	portStr := targetPort[lastDash+1:]

	if name == "" || portStr == "" {
		return nil, false
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, false
	}

	cfg := &config.K8sRouteConfig{
		Context:   context,
		Namespace: namespace,
		Port:      port,
		Scheme:    scheme,
	}

	if isService {
		cfg.Service = name
	} else {
		cfg.Pod = name
	}

	return cfg, true
}
