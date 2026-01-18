package tunnelmgr

import (
	"fmt"
	"log"
	"time"
)

func (m *Manager) GetOrCreateTunnel(hostname string, scheme string) (TunnelHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tunnel, ok := m.tunnels[hostname]; ok {
		if tunnel.IsRunning() {
			tunnel.Touch()
			return tunnel, nil
		}
		// dead tunnel, clean it up
		delete(m.tunnels, hostname)
	}

	// static routes take priority, then try dynamic pattern matching
	routeConfig, ok := m.config.HTTP.K8s.Routes[hostname]
	if !ok {
		if parsed, valid := ParseDynamicHostname(hostname, m.config.HTTP.K8s.DynamicHost, scheme); valid {
			routeConfig = *parsed
			ok = true
			log.Printf("[dynamic] Resolved %s -> %s/%s:%d (context: %s)",
				hostname, routeConfig.Namespace, routeConfig.Service+routeConfig.Pod, routeConfig.Port, routeConfig.Context)
		}
	}
	if !ok {
		return nil, fmt.Errorf("no route configured for hostname: %s", hostname)
	}

	clientset, restConfig, err := m.clientFactory.GetClientForContext(m.config.HTTP.K8s.ResolvedKubeconfigs, routeConfig.Context)
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s client for context %s: %w", routeConfig.Context, err)
	}

	tun := m.tunnelFactory(hostname, routeConfig, clientset, restConfig, m.config.HTTP.ListenAddr, m.config.Verbose)
	m.tunnels[hostname] = tun

	return tun, nil
}

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

func (m *Manager) cleanupIdleTunnels() {
	m.cleanupIdleHTTPTunnels()
	m.cleanupIdleTCPTunnels()
}

func (m *Manager) cleanupIdleHTTPTunnels() {
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

func (m *Manager) cleanupIdleTCPTunnels() {
	m.tcpTunnelsMu.Lock()
	defer m.tcpTunnelsMu.Unlock()

	// TCP idle timeout falls back to HTTP idle timeout if not specified
	tcpIdleTimeout := m.config.TCP.IdleTimeout
	if tcpIdleTimeout == 0 {
		tcpIdleTimeout = m.config.HTTP.IdleTimeout
	}

	for port, tunnel := range m.tcpTunnels {
		if tunnel.IsRunning() && tunnel.IdleDuration() > tcpIdleTimeout {
			target := m.config.TCP.K8s.Routes[port]
			idleDur := tunnel.IdleDuration().Round(time.Second)
			log.Printf("Tunnel stopped: tcp://localhost:%d -> %s/%s (idle for %v)",
				port, target.Namespace, target.TargetName(), idleDur)
			tunnel.Stop()
			delete(m.tcpTunnels, port)
		}
	}
}
