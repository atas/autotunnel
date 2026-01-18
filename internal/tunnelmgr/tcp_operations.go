package tunnelmgr

import (
	"fmt"
	"log"

	"github.com/atas/autotunnel/internal/config"
)

func (m *Manager) GetOrCreateTCPTunnel(localPort int) (TunnelHandle, error) {
	m.tcpTunnelsMu.Lock()
	defer m.tcpTunnelsMu.Unlock()

	if tunnel, ok := m.tcpTunnels[localPort]; ok {
		if tunnel.IsRunning() {
			tunnel.Touch()
			return tunnel, nil
		}
		delete(m.tcpTunnels, localPort)
	}

	routeConfig, ok := m.config.TCP.K8s.Routes[localPort]
	if !ok {
		return nil, fmt.Errorf("no TCP route configured for port %d", localPort)
	}

	kubeconfigs := m.config.TCP.K8s.ResolvedKubeconfigs
	if len(kubeconfigs) == 0 {
		kubeconfigs = m.config.HTTP.K8s.ResolvedKubeconfigs
	}

	clientset, restConfig, err := m.clientFactory.GetClientForContext(kubeconfigs, routeConfig.Context)
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s client for context %s: %w", routeConfig.Context, err)
	}

	// tunnel code is shared between HTTP and TCP, so we convert to the common type
	k8sRoute := config.K8sRouteConfig{
		Context:   routeConfig.Context,
		Namespace: routeConfig.Namespace,
		Service:   routeConfig.Service,
		Pod:       routeConfig.Pod,
		Port:      routeConfig.Port,
		Scheme:    "tcp",
	}

	tunnelID := fmt.Sprintf("tcp:%d", localPort)
	tunnel := m.tunnelFactory(
		tunnelID,
		k8sRoute,
		clientset,
		restConfig,
		"", // No listen addr for tunnels - they pick a random port
		m.config.Verbose,
	)

	m.tcpTunnels[localPort] = tunnel

	if m.config.Verbose {
		log.Printf("[tcp] Created tunnel for port %d -> %s/%s:%d",
			localPort, routeConfig.Namespace, tcpTarget(routeConfig), routeConfig.Port)
	}

	return tunnel, nil
}

func tcpTarget(route config.TCPRouteConfig) string {
	if route.Service != "" {
		return route.Service
	}
	return route.Pod
}
