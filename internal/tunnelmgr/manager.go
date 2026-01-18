package tunnelmgr

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/k8sutil"
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

	tunnels      map[string]TunnelHandle // HTTP: hostname -> tunnel
	tcpTunnels   map[int]TunnelHandle    // TCP: local port -> tunnel
	tcpTunnelsMu sync.RWMutex

	tunnelFactory TunnelFactory

	clientFactory *k8sutil.ClientFactory

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
		clientFactory: k8sutil.NewClientFactory(cfg.Verbose),
		ctx:           ctx,
		cancel:        cancel,
	}
}

func (m *Manager) Start() {
	m.wg.Add(1)
	go m.idleCleanupLoop()
	fmt.Printf("Idle timeout: %v\n", m.config.HTTP.IdleTimeout)

	// Print TCP idle timeout if different from HTTP
	if m.config.TCP.IdleTimeout > 0 && m.config.TCP.IdleTimeout != m.config.HTTP.IdleTimeout {
		fmt.Printf("TCP idle timeout: %v\n", m.config.TCP.IdleTimeout)
	}
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

	m.clientFactory.Clear()

	m.wg.Wait()
	log.Println("Tunnel manager stopped")
}

// GetClientForContext is exposed so tcpserver's jump handler can reuse our k8s clients
func (m *Manager) GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	return m.clientFactory.GetClientForContext(kubeconfigPaths, contextName)
}

// ClientFactory returns the client factory (for testing)
func (m *Manager) ClientFactory() *k8sutil.ClientFactory {
	return m.clientFactory
}
