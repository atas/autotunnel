package tunnelmgr

import (
	"context"
	"time"

	"github.com/atas/autotunnel/internal/tunnel"
)

// TunnelHandle provides access to tunnel operations needed by the HTTP server and manager
type TunnelHandle interface {
	IsRunning() bool
	Start(ctx context.Context) error
	Stop()
	LocalPort() int
	Scheme() string
	Touch()
	IdleDuration() time.Duration
	State() tunnel.State
}

// TunnelInfo contains information about a tunnel for display purposes
type TunnelInfo struct {
	Hostname     string
	LocalPort    int
	State        string
	IdleDuration time.Duration
}

// ActiveTunnels returns the count of currently active tunnels
func (m *Manager) ActiveTunnels() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, tunnel := range m.tunnels {
		if tunnel.IsRunning() {
			count++
		}
	}
	return count
}

// ListTunnels returns information about all tunnels
func (m *Manager) ListTunnels() []TunnelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]TunnelInfo, 0, len(m.tunnels))
	for hostname, tunnel := range m.tunnels {
		infos = append(infos, TunnelInfo{
			Hostname:     hostname,
			LocalPort:    tunnel.LocalPort(),
			State:        tunnel.State().String(),
			IdleDuration: tunnel.IdleDuration(),
		})
	}
	return infos
}
