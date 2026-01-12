package tunnel

import (
	"sync"
	"testing"

	"github.com/atas/autotunnel/internal/config"
)

func TestState_String(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateIdle, "idle"},
		{StateStarting, "starting"},
		{StateRunning, "running"},
		{StateStopping, "stopping"},
		{StateFailed, "failed"},
		{State(99), "unknown"}, // Unknown state
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := tt.state.String()
			if got != tt.expected {
				t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.expected)
			}
		})
	}
}

func TestNewTunnel(t *testing.T) {
	cfg := config.K8sRouteConfig{
		Namespace: "test-ns",
		Service:   "test-svc",
		Port:      8080,
		Scheme:    "https",
	}

	tunnel := NewTunnel("test.localhost", cfg, nil, nil, ":8989", true)

	if tunnel.hostname != "test.localhost" {
		t.Errorf("hostname = %q, want %q", tunnel.hostname, "test.localhost")
	}
	if tunnel.config.Namespace != "test-ns" {
		t.Errorf("config.Namespace = %q, want %q", tunnel.config.Namespace, "test-ns")
	}
	if tunnel.config.Service != "test-svc" {
		t.Errorf("config.Service = %q, want %q", tunnel.config.Service, "test-svc")
	}
	if tunnel.config.Port != 8080 {
		t.Errorf("config.Port = %d, want %d", tunnel.config.Port, 8080)
	}
	if tunnel.listenAddr != ":8989" {
		t.Errorf("listenAddr = %q, want %q", tunnel.listenAddr, ":8989")
	}
	if tunnel.verbose != true {
		t.Errorf("verbose = %v, want %v", tunnel.verbose, true)
	}
	if tunnel.state != StateIdle {
		t.Errorf("state = %v, want %v", tunnel.state, StateIdle)
	}
	if tunnel.lastAccess.IsZero() {
		t.Error("lastAccess should not be zero")
	}
}

func TestTunnel_State(t *testing.T) {
	tunnel := &Tunnel{state: StateRunning}

	if got := tunnel.State(); got != StateRunning {
		t.Errorf("State() = %v, want %v", got, StateRunning)
	}
}

func TestTunnel_IsRunning(t *testing.T) {
	tests := []struct {
		name     string
		state    State
		expected bool
	}{
		{"idle", StateIdle, false},
		{"starting", StateStarting, false},
		{"running", StateRunning, true},
		{"stopping", StateStopping, false},
		{"failed", StateFailed, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnel := &Tunnel{state: tt.state}
			if got := tunnel.IsRunning(); got != tt.expected {
				t.Errorf("IsRunning() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestTunnel_Stop_NotRunning(t *testing.T) {
	tunnel := &Tunnel{state: StateIdle}
	tunnel.Stop() // Should not panic

	if tunnel.state != StateIdle {
		t.Errorf("state = %v, want %v (should remain unchanged)", tunnel.state, StateIdle)
	}
}

func TestTunnel_Stop_Running(t *testing.T) {
	stopChan := make(chan struct{})
	tunnel := &Tunnel{
		state:    StateRunning,
		stopChan: stopChan,
	}

	tunnel.Stop()

	// Verify stopChan was closed
	select {
	case <-stopChan:
		// Expected: channel was closed
	default:
		t.Error("stopChan should have been closed")
	}

	if tunnel.state != StateIdle {
		t.Errorf("state = %v, want %v", tunnel.state, StateIdle)
	}
}

func TestTunnel_ConcurrentStateAccess(t *testing.T) {
	tunnel := NewTunnel("test.localhost", config.K8sRouteConfig{}, nil, nil, ":8989", false)

	var wg sync.WaitGroup
	const goroutines = 100

	// Concurrent reads
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tunnel.State()
			_ = tunnel.IsRunning()
		}()
	}

	wg.Wait()
}
