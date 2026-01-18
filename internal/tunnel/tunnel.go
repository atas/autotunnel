package tunnel

import (
	"context"
	"sync"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

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

type Tunnel struct {
	mu sync.RWMutex

	hostname   string
	config     config.K8sRouteConfig
	listenAddr string
	verbose    bool

	clientset  kubernetes.Interface
	restConfig *rest.Config

	state      State
	localPort  int
	lastAccess time.Time

	stopChan  chan struct{}
	readyChan chan struct{}

	lastError error
}

func NewTunnel(hostname string, cfg config.K8sRouteConfig, clientset kubernetes.Interface, restConfig *rest.Config, listenAddr string, verbose bool) *Tunnel {
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

func (t *Tunnel) IsRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == StateRunning
}

func (t *Tunnel) State() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}
