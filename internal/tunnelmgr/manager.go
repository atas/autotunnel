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

// TunnelFactory is injectable for testing - swap in mocks instead of real tunnels
type TunnelFactory func(hostname string, cfg config.K8sRouteConfig,
	clientset kubernetes.Interface, restConfig *rest.Config,
	listenAddr string, verbose bool) TunnelHandle

func defaultTunnelFactory(hostname string, cfg config.K8sRouteConfig,
	clientset kubernetes.Interface, restConfig *rest.Config,
	listenAddr string, verbose bool) TunnelHandle {
	return tunnel.NewTunnel(hostname, cfg, clientset, restConfig, listenAddr, verbose)
}

type Manager struct {
	mu sync.RWMutex

	config *config.Config

	tunnels    map[string]TunnelHandle // HTTP: hostname -> tunnel
	tcpTunnels map[int]TunnelHandle    // TCP: local port -> tunnel
	tcpTunnelsMu sync.RWMutex

	tunnelFactory TunnelFactory

	k8sClients   map[string]*k8sClient // one client per k8s context
	k8sClientsMu sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewManager(cfg *config.Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		config:        cfg,
		tunnels:       make(map[string]TunnelHandle),
		tcpTunnels:    make(map[int]TunnelHandle),
		tunnelFactory: defaultTunnelFactory,
		k8sClients:    make(map[string]*k8sClient),
		ctx:           ctx,
		cancel:        cancel,
	}
}

func (m *Manager) Start() {
	m.wg.Add(1)
	go m.idleCleanupLoop()
	fmt.Printf("Idle timeout: %v\n", m.config.HTTP.IdleTimeout)
}

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

	m.tcpTunnelsMu.Lock()
	for port, tunnel := range m.tcpTunnels {
		if tunnel.IsRunning() {
			log.Printf("Stopping TCP tunnel for port %d", port)
			tunnel.Stop()
		}
	}
	m.tcpTunnels = make(map[int]TunnelHandle)
	m.tcpTunnelsMu.Unlock()

	m.k8sClientsMu.Lock()
	m.k8sClients = make(map[string]*k8sClient)
	m.k8sClientsMu.Unlock()

	m.wg.Wait()
	log.Println("Tunnel manager stopped")
}

func (m *Manager) UpdateConfig(newConfig *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldRoutes := m.config.HTTP.K8s.Routes
	newRoutes := newConfig.HTTP.K8s.Routes

	// tear down HTTP tunnels for routes that no longer exist
	for hostname := range oldRoutes {
		if _, exists := newRoutes[hostname]; !exists {
			if tunnel, ok := m.tunnels[hostname]; ok {
				log.Printf("Route removed, stopping tunnel: %s", hostname)
				tunnel.Stop()
				delete(m.tunnels, hostname)
			}
		}
	}

	// tear down TCP tunnels for routes that no longer exist
	m.tcpTunnelsMu.Lock()
	oldTCPRoutes := m.config.TCP.K8s.Routes
	newTCPRoutes := newConfig.TCP.K8s.Routes
	for port := range oldTCPRoutes {
		if _, exists := newTCPRoutes[port]; !exists {
			if tunnel, ok := m.tcpTunnels[port]; ok {
				log.Printf("TCP route removed, stopping tunnel for port: %d", port)
				tunnel.Stop()
				delete(m.tcpTunnels, port)
			}
		}
	}
	m.tcpTunnelsMu.Unlock()

	m.config = newConfig
}


// GetClientForContext is exposed so tcpserver's jump handler can reuse our k8s clients
func (m *Manager) GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	return m.getClientsetAndConfig(kubeconfigPaths, contextName)
}
