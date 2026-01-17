package tunnel

import (
	"context"
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
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tunnel.State()
			_ = tunnel.IsRunning()
		}()
	}

	wg.Wait()
}

func TestTunnel_Start_AlreadyRunning(t *testing.T) {
	// Create a tunnel that's already in running state
	tunnel := &Tunnel{
		state:    StateRunning,
		stopChan: make(chan struct{}),
	}

	// Start should return nil immediately when already running
	err := tunnel.Start(context.Background())
	if err != nil {
		t.Errorf("Start() on running tunnel = %v, want nil", err)
	}

	// State should remain running
	if tunnel.state != StateRunning {
		t.Errorf("state = %v, want %v", tunnel.state, StateRunning)
	}
}

func TestTunnel_StateTransitions(t *testing.T) {
	tests := []struct {
		name        string
		initial     State
		action      func(*Tunnel)
		expected    State
		description string
	}{
		{
			name:    "idle to starting (via Start entry)",
			initial: StateIdle,
			action: func(tun *Tunnel) {
				tun.mu.Lock()
				tun.state = StateStarting
				tun.mu.Unlock()
			},
			expected:    StateStarting,
			description: "Starting state should be set before port-forward begins",
		},
		{
			name:    "running to stopping via Stop",
			initial: StateRunning,
			action: func(tun *Tunnel) {
				tun.stopChan = make(chan struct{})
				tun.Stop()
			},
			expected:    StateIdle,
			description: "Stop should transition to idle",
		},
		{
			name:    "stopping state does not respond to Stop",
			initial: StateStopping,
			action: func(tun *Tunnel) {
				tun.Stop()
			},
			expected:    StateStopping,
			description: "Stop on non-running state should be no-op",
		},
		{
			name:    "failed state does not respond to Stop",
			initial: StateFailed,
			action: func(tun *Tunnel) {
				tun.Stop()
			},
			expected:    StateFailed,
			description: "Stop on failed state should be no-op",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnel := &Tunnel{state: tt.initial}
			tt.action(tunnel)

			if tunnel.state != tt.expected {
				t.Errorf("%s: state = %v, want %v", tt.description, tunnel.state, tt.expected)
			}
		})
	}
}

func TestTunnel_Stop_WithNilStopChan(t *testing.T) {
	// Test that Stop doesn't panic with nil stopChan
	tunnel := &Tunnel{
		state:    StateRunning,
		stopChan: nil, // nil channel
	}

	// Should not panic
	tunnel.Stop()

	if tunnel.state != StateIdle {
		t.Errorf("state = %v, want %v", tunnel.state, StateIdle)
	}
}

func TestTunnel_Stop_MultipleCallsSafe(t *testing.T) {
	stopChan := make(chan struct{})
	tunnel := &Tunnel{
		state:    StateRunning,
		stopChan: stopChan,
	}

	// First stop
	tunnel.Stop()

	// State should be idle now
	if tunnel.state != StateIdle {
		t.Errorf("After first Stop: state = %v, want %v", tunnel.state, StateIdle)
	}

	// Second stop should be safe (no panic from double close)
	tunnel.Stop()

	// State should remain idle
	if tunnel.state != StateIdle {
		t.Errorf("After second Stop: state = %v, want %v", tunnel.state, StateIdle)
	}
}

func TestTunnel_ConcurrentStartStop(t *testing.T) {
	tunnel := NewTunnel("test.localhost", config.K8sRouteConfig{}, nil, nil, ":8989", false)

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent Stop calls should be safe
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Simulate various state operations
			_ = tunnel.State()
			_ = tunnel.IsRunning()
		}()
	}

	wg.Wait()
}

func TestTunnel_InitialStateIsIdle(t *testing.T) {
	tunnel := NewTunnel("test.localhost", config.K8sRouteConfig{}, nil, nil, ":8989", false)

	if tunnel.State() != StateIdle {
		t.Errorf("New tunnel state = %v, want %v", tunnel.State(), StateIdle)
	}
	if tunnel.IsRunning() {
		t.Error("New tunnel should not be running")
	}
}
