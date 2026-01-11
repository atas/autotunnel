package tunnel

import (
	"context"
	"sync"
	"time"

	"github.com/atas/lazyfwd/internal/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// State represents the current state of a tunnel
type State int

const (
	StateIdle State = iota
	StateStarting
	StateRunning
	StateStopping
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Tunnel manages a single port-forward connection to a Kubernetes service or pod
type Tunnel struct {
	mu sync.RWMutex

	// Configuration
	hostname   string
	config     config.K8sRouteConfig
	listenAddr string
	verbose    bool

	// Shared k8s resources (from Manager)
	clientset  *kubernetes.Clientset
	restConfig *rest.Config

	// State
	state      State
	localPort  int
	lastAccess time.Time

	// Port-forward resources
	stopChan  chan struct{}
	readyChan chan struct{}

	// Error tracking
	lastError error
}

// NewTunnel creates a new tunnel instance
func NewTunnel(hostname string, cfg config.K8sRouteConfig, clientset *kubernetes.Clientset, restConfig *rest.Config, listenAddr string, verbose bool) *Tunnel {
	return &Tunnel{
		hostname:   hostname,
		config:     cfg,
		clientset:  clientset,
		restConfig: restConfig,
		listenAddr: listenAddr,
		verbose:    verbose,
		state:      StateIdle,
		lastAccess: time.Now(),
	}
}

// Start initiates the port-forward connection
func (t *Tunnel) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.state == StateRunning {
		t.mu.Unlock()
		return nil
	}
	t.state = StateStarting
	t.mu.Unlock()

	return t.startPortForward(ctx)
}

// Stop gracefully terminates the port-forward
func (t *Tunnel) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state != StateRunning {
		return
	}

	t.state = StateStopping
	if t.stopChan != nil {
		close(t.stopChan)
	}
	t.state = StateIdle
}

// IsRunning returns true if the tunnel is active
func (t *Tunnel) IsRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == StateRunning
}

// State returns the current tunnel state
func (t *Tunnel) State() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}
