package tunnelmgr

import (
	"fmt"
	"log"
	"time"
)

// GetOrCreateTunnel returns an existing tunnel or creates a new one
func (m *Manager) GetOrCreateTunnel(hostname string) (TunnelHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if tunnel exists and is running
	if tunnel, ok := m.tunnels[hostname]; ok {
		if tunnel.IsRunning() {
			tunnel.Touch()
			return tunnel, nil
		}
		// Tunnel exists but not running, remove it
		delete(m.tunnels, hostname)
	}

	// Look up route config
	routeConfig, ok := m.config.HTTP.K8s.Routes[hostname]
	if !ok {
		return nil, fmt.Errorf("no route configured for hostname: %s", hostname)
	}

	// Get or create shared k8s client for this context
	clientset, restConfig, err := m.getClientsetAndConfig(m.config.HTTP.K8s.Kubeconfig, routeConfig.Context)
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s client for context %s: %w", routeConfig.Context, err)
	}

	// Create new tunnel with shared k8s resources
	tun := m.tunnelFactory(hostname, routeConfig, clientset, restConfig, m.config.HTTP.ListenAddr, m.config.Verbose)
	m.tunnels[hostname] = tun

	return tun, nil
}

// idleCleanupLoop periodically checks for and closes idle tunnels
func (m *Manager) idleCleanupLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.cleanupIdleTunnels()
		}
	}
}

// cleanupIdleTunnels closes tunnels that have exceeded the idle timeout
func (m *Manager) cleanupIdleTunnels() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for hostname, tunnel := range m.tunnels {
		if tunnel.IsRunning() && tunnel.IdleDuration() > m.config.HTTP.IdleTimeout {
			idleDur := tunnel.IdleDuration().Round(time.Second)
			log.Printf("Tunnel stopped: %s://%s%s (idle for %v)",
				tunnel.Scheme(), hostname, m.config.HTTP.ListenAddr, idleDur)
			tunnel.Stop()
			delete(m.tunnels, hostname)
		}
	}
}
