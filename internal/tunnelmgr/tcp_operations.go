package tunnelmgr

import (
	"fmt"
	"log"

	"github.com/atas/autotunnel/internal/tunnel"
)

func (m *Manager) GetOrCreateTCPTunnel(localPort int) (TunnelHandle, error) {
	m.tcpTunnelsMu.Lock()
	defer m.tcpTunnelsMu.Unlock()

	if tun, ok := m.tcpTunnels[localPort]; ok {
		state := tun.State()
		// Preserve tunnels that are idle, starting, or running
		if state != tunnel.StateStopping && state != tunnel.StateFailed {
			tun.Touch()
			return tun, nil
		}
		// Only delete stopped/failed tunnels
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

	tunnelID := fmt.Sprintf("tcp:%d", localPort)
	newTunnel := m.tunnelFactory(
		tunnelID,
		routeConfig.ToK8sRouteConfig(),
		clientset,
		restConfig,
		"", // No listen addr for tunnels - they pick a random port
		m.config.Verbose,
	)

	m.tcpTunnels[localPort] = newTunnel

	if m.config.Verbose {
		log.Printf("[tcp] Created tunnel for port %d -> %s/%s:%d",
			localPort, routeConfig.Namespace, routeConfig.TargetName(), routeConfig.Port)
	}

	return newTunnel, nil
}
