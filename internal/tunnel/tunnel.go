package tunnel

import (
	"context"
	"errors"
	"fmt"
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

	state       State
	localPort   int
	lastAccess  time.Time
	activeConns int

	// stopChan stops the current port-forward. It is only touched with mu held
	// and is nil once closed, so it cannot be closed twice.
	stopChan chan struct{}

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

// ErrFailed is what Start returns for a tunnel that has failed. The manager has replaced it, so
// a caller holding it asks the manager again.
var ErrFailed = errors.New("tunnel already failed")

func (t *Tunnel) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.state == StateRunning {
		t.mu.Unlock()
		return nil
	}
	if t.state == StateStarting {
		t.mu.Unlock()
		return t.awaitReady(ctx) // Wait for first caller to complete
	}
	if t.state == StateFailed {
		// A failed tunnel stays failed and the manager replaces it. Restarting it
		// here could bring back an object the manager has already dropped.
		err := t.lastError
		t.mu.Unlock()
		if err == nil {
			return ErrFailed
		}
		return fmt.Errorf("%w: %w", ErrFailed, err)
	}
	t.state = StateStarting
	t.mu.Unlock()
	return t.startPortForward(ctx)
}

// awaitReady waits for a concurrent Start() to complete
func (t *Tunnel) awaitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			t.mu.RLock()
			state := t.state
			lastErr := t.lastError
			t.mu.RUnlock()

			switch state {
			case StateRunning:
				return nil // Success
			case StateFailed:
				return lastErr
			case StateIdle:
				return t.Start(ctx) // First caller failed, retry
				// StateStarting: keep waiting
			}
		}
	}
}

func (t *Tunnel) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state != StateRunning {
		return
	}

	t.state = StateStopping
	t.closeStopChan()
	t.state = StateIdle
}

// closeStopChan stops the current port-forward. The caller must hold mu.
func (t *Tunnel) closeStopChan() {
	if t.stopChan != nil {
		close(t.stopChan)
		t.stopChan = nil
	}
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
