package internal

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Manager handles the lifecycle of all tunnels
type Manager struct {
	mu sync.RWMutex

	// Configuration
	config *Config

	// Active tunnels keyed by hostname
	tunnels map[string]*Tunnel

	// Shutdown coordination
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewManager creates a new tunnel manager
func NewManager(config *Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		config:  config,
		tunnels: make(map[string]*Tunnel),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// GetOrCreateTunnel returns an existing tunnel or creates a new one
func (m *Manager) GetOrCreateTunnel(hostname string) (*Tunnel, error) {
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

	// Create new tunnel
	tunnel := NewTunnel(hostname, routeConfig, m.config.HTTP.K8s.Kubeconfig, m.config.HTTP.ListenAddr, m.config.Verbose)
	m.tunnels[hostname] = tunnel

	return tunnel, nil
}

// Start begins the manager's background tasks
func (m *Manager) Start() {
	m.wg.Add(1)
	go m.idleCleanupLoop()
	fmt.Printf("Idle timeout: %v\n", m.config.HTTP.IdleTimeout)
}

// Shutdown gracefully stops all tunnels
func (m *Manager) Shutdown() {
	log.Println("Shutting down tunnel manager...")
	m.cancel()

	m.mu.Lock()
	for hostname, tunnel := range m.tunnels {
		if tunnel.IsRunning() {
			log.Printf("Stopping tunnel for %s", hostname)
			tunnel.Stop()
		}
	}
	m.tunnels = make(map[string]*Tunnel)
	m.mu.Unlock()

	m.wg.Wait()
	log.Println("Tunnel manager stopped")
}

// UpdateConfig updates the manager's configuration and cleans up removed routes
func (m *Manager) UpdateConfig(newConfig *Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldRoutes := m.config.HTTP.K8s.Routes
	newRoutes := newConfig.HTTP.K8s.Routes

	// Stop tunnels for removed routes
	for hostname := range oldRoutes {
		if _, exists := newRoutes[hostname]; !exists {
			if tunnel, ok := m.tunnels[hostname]; ok {
				log.Printf("Route removed, stopping tunnel: %s", hostname)
				tunnel.Stop()
				delete(m.tunnels, hostname)
			}
		}
	}

	m.config = newConfig
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

// TunnelInfo contains information about a tunnel for display purposes
type TunnelInfo struct {
	Hostname     string
	LocalPort    int
	State        string
	IdleDuration time.Duration
}
