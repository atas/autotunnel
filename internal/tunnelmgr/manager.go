package tunnelmgr

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/tunnel"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TunnelFactory creates a new tunnel instance
type TunnelFactory func(hostname string, cfg config.K8sRouteConfig,
	clientset kubernetes.Interface, restConfig *rest.Config,
	listenAddr string, verbose bool) TunnelHandle

// defaultTunnelFactory creates real tunnel instances
func defaultTunnelFactory(hostname string, cfg config.K8sRouteConfig,
	clientset kubernetes.Interface, restConfig *rest.Config,
	listenAddr string, verbose bool) TunnelHandle {
	return tunnel.NewTunnel(hostname, cfg, clientset, restConfig, listenAddr, verbose)
}

// Manager handles the lifecycle of all tunnels
type Manager struct {
	mu sync.RWMutex

	// Configuration
	config *config.Config

	// Active tunnels keyed by hostname
	tunnels map[string]TunnelHandle

	// Factory for creating tunnels (enables testing with mocks)
	tunnelFactory TunnelFactory

	// Cached k8s clients per context name
	k8sClients   map[string]*k8sClient
	k8sClientsMu sync.RWMutex

	// Shutdown coordination
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewManager creates a new tunnel manager
func NewManager(cfg *config.Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		config:        cfg,
		tunnels:       make(map[string]TunnelHandle),
		tunnelFactory: defaultTunnelFactory,
		k8sClients:    make(map[string]*k8sClient),
		ctx:           ctx,
		cancel:        cancel,
	}
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
	m.tunnels = make(map[string]TunnelHandle)
	m.mu.Unlock()

	// Clear cached k8s clients
	m.k8sClientsMu.Lock()
	m.k8sClients = make(map[string]*k8sClient)
	m.k8sClientsMu.Unlock()

	m.wg.Wait()
	log.Println("Tunnel manager stopped")
}

// UpdateConfig updates the manager's configuration and cleans up removed routes
func (m *Manager) UpdateConfig(newConfig *config.Config) {
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
