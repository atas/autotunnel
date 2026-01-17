package tcpserver

import (
	"net"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/tunnelmgr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// mockManager implements Manager interface for testing
type mockManager struct {
	getTCPTunnelCalls []int
	tunnelToReturn    tunnelmgr.TunnelHandle
	errorToReturn     error
}

func (m *mockManager) GetOrCreateTCPTunnel(port int) (tunnelmgr.TunnelHandle, error) {
	m.getTCPTunnelCalls = append(m.getTCPTunnelCalls, port)
	return m.tunnelToReturn, m.errorToReturn
}

func (m *mockManager) GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	// Return nil for testing - socat tests would need more elaborate mocking
	return nil, nil, nil
}

// testConfig creates a config with TCP routes on high ports
func testConfig(tcpRoutes map[int]config.TCPRouteConfig) *config.Config {
	return &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: tcpRoutes,
			},
		},
	}
}

func TestNewServer(t *testing.T) {
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19100: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
	})
	mgr := &mockManager{}

	s := NewServer(cfg, mgr)

	if s.config != cfg {
		t.Error("Expected config to be set")
	}
	if s.manager != mgr {
		t.Error("Expected manager to be set")
	}
	if s.listeners == nil {
		t.Error("Expected listeners map to be initialized")
	}
	if s.ctx == nil {
		t.Error("Expected context to be set")
	}
}

func TestServer_StartShutdown(t *testing.T) {
	// Use high ports to avoid conflicts
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19100: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
		19101: {Context: "test", Namespace: "ns", Service: "svc2", Port: 81},
	})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	// Start server
	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Verify listeners were created
	s.mu.RLock()
	if len(s.listeners) != 2 {
		t.Errorf("Expected 2 listeners, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Note: We don't test actual connections here because that would require
	// a fully mocked tunnel chain. We just verify listeners exist and ports are bound.

	// Shutdown
	s.Shutdown()

	// Verify listeners were stopped
	s.mu.RLock()
	if len(s.listeners) != 0 {
		t.Errorf("Expected 0 listeners after shutdown, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Verify ports are released (connection should fail)
	_, err = net.DialTimeout("tcp", "127.0.0.1:19100", 100*time.Millisecond)
	if err == nil {
		t.Error("Expected connection to fail after shutdown")
	}
}

func TestServer_UpdateConfig_AddsNewPorts(t *testing.T) {
	// Start with one route
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19200: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
	})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Shutdown()

	// Verify initial state
	s.mu.RLock()
	if len(s.listeners) != 1 {
		t.Fatalf("Expected 1 listener initially, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Update config with additional route
	newCfg := testConfig(map[int]config.TCPRouteConfig{
		19200: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
		19201: {Context: "test", Namespace: "ns", Service: "svc2", Port: 81},
	})
	s.UpdateConfig(newCfg)

	// Verify new listener was added
	s.mu.RLock()
	if len(s.listeners) != 2 {
		t.Errorf("Expected 2 listeners after update, got %d", len(s.listeners))
	}
	if _, exists := s.listeners[19201]; !exists {
		t.Error("Expected listener for port 19201 to exist")
	}
	s.mu.RUnlock()

	// Note: We don't test actual connections because that requires a fully mocked tunnel chain
}

func TestServer_UpdateConfig_RemovesPorts(t *testing.T) {
	// Start with two routes
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19300: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
		19301: {Context: "test", Namespace: "ns", Service: "svc2", Port: 81},
	})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Shutdown()

	// Verify initial state
	s.mu.RLock()
	if len(s.listeners) != 2 {
		t.Fatalf("Expected 2 listeners initially, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Update config to remove one route
	newCfg := testConfig(map[int]config.TCPRouteConfig{
		19300: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
		// 19301 removed
	})
	s.UpdateConfig(newCfg)

	// Verify listener was removed
	s.mu.RLock()
	if len(s.listeners) != 1 {
		t.Errorf("Expected 1 listener after update, got %d", len(s.listeners))
	}
	if _, exists := s.listeners[19301]; exists {
		t.Error("Expected listener for port 19301 to be removed")
	}
	if _, exists := s.listeners[19300]; !exists {
		t.Error("Expected listener for port 19300 to still exist")
	}
	s.mu.RUnlock()

	// Verify removed port is released (connection should fail)
	_, err = net.DialTimeout("tcp", "127.0.0.1:19301", 100*time.Millisecond)
	if err == nil {
		t.Error("Expected connection to removed port 19301 to fail")
	}

	// Note: We don't test connecting to kept port because that requires a fully mocked tunnel chain
}

func TestServer_Start_PortConflict(t *testing.T) {
	// First, bind a port manually
	listener, err := net.Listen("tcp", "127.0.0.1:19400")
	if err != nil {
		t.Fatalf("Failed to bind test port: %v", err)
	}
	defer listener.Close()

	// Try to start server on the same port
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19400: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
	})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err = s.Start()
	if err == nil {
		s.Shutdown()
		t.Error("Expected error when starting server on occupied port")
	}
}

func TestServer_Start_NoRoutes(t *testing.T) {
	// Server with no routes should start successfully
	cfg := testConfig(map[int]config.TCPRouteConfig{})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server with no routes: %v", err)
	}
	defer s.Shutdown()

	s.mu.RLock()
	if len(s.listeners) != 0 {
		t.Errorf("Expected 0 listeners for empty routes, got %d", len(s.listeners))
	}
	s.mu.RUnlock()
}

func TestServer_Shutdown_MultipleCallsSafe(t *testing.T) {
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19500: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
	})
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Multiple shutdown calls should be safe
	s.Shutdown()
	s.Shutdown()
	s.Shutdown()

	// After shutdown, listeners should be empty
	s.mu.RLock()
	if len(s.listeners) != 0 {
		t.Errorf("Expected 0 listeners after shutdown, got %d", len(s.listeners))
	}
	s.mu.RUnlock()
}

func TestServer_Start_MixedRoutesAndSocat(t *testing.T) {
	// Test config with both regular routes and socat routes
	cfg := &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: map[int]config.TCPRouteConfig{
					19600: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
				},
				Socat: map[int]config.SocatRouteConfig{
					19601: {
						Context:   "test",
						Namespace: "ns",
						Via: config.ViaConfig{
							Pod: "jump-pod",
						},
						Target: config.TargetConfig{
							Host: "database.internal",
							Port: 5432,
						},
					},
				},
			},
		},
	}
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server with mixed routes: %v", err)
	}
	defer s.Shutdown()

	// Should have both listeners
	s.mu.RLock()
	if len(s.listeners) != 2 {
		t.Errorf("Expected 2 listeners (route + socat), got %d", len(s.listeners))
	}
	// Verify route listener
	if pl, exists := s.listeners[19600]; !exists {
		t.Error("Expected listener for route port 19600")
	} else if pl.listenerType != listenerTypeRoute {
		t.Errorf("Port 19600 should be route type, got %v", pl.listenerType)
	}
	// Verify socat listener
	if pl, exists := s.listeners[19601]; !exists {
		t.Error("Expected listener for socat port 19601")
	} else if pl.listenerType != listenerTypeSocat {
		t.Errorf("Port 19601 should be socat type, got %v", pl.listenerType)
	}
	s.mu.RUnlock()
}

func TestServer_UpdateConfig_AddsSocatPorts(t *testing.T) {
	// Start with no socat routes
	cfg := &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: map[int]config.TCPRouteConfig{
					19700: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
				},
				Socat: map[int]config.SocatRouteConfig{},
			},
		},
	}
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Shutdown()

	// Verify initial state
	s.mu.RLock()
	if len(s.listeners) != 1 {
		t.Fatalf("Expected 1 listener initially, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Update config to add a socat route
	newCfg := &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: map[int]config.TCPRouteConfig{
					19700: {Context: "test", Namespace: "ns", Service: "svc1", Port: 80},
				},
				Socat: map[int]config.SocatRouteConfig{
					19701: {
						Context:   "test",
						Namespace: "ns",
						Via:       config.ViaConfig{Pod: "jump-pod"},
						Target:    config.TargetConfig{Host: "db.internal", Port: 5432},
					},
				},
			},
		},
	}
	s.UpdateConfig(newCfg)

	// Verify new socat listener was added
	s.mu.RLock()
	if len(s.listeners) != 2 {
		t.Errorf("Expected 2 listeners after adding socat, got %d", len(s.listeners))
	}
	if pl, exists := s.listeners[19701]; !exists {
		t.Error("Expected listener for socat port 19701")
	} else if pl.listenerType != listenerTypeSocat {
		t.Errorf("Port 19701 should be socat type")
	}
	s.mu.RUnlock()
}

func TestServer_UpdateConfig_RemovesSocatPorts(t *testing.T) {
	// Start with a socat route
	cfg := &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: map[int]config.TCPRouteConfig{},
				Socat: map[int]config.SocatRouteConfig{
					19800: {
						Context:   "test",
						Namespace: "ns",
						Via:       config.ViaConfig{Pod: "jump-pod"},
						Target:    config.TargetConfig{Host: "db.internal", Port: 5432},
					},
				},
			},
		},
	}
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Shutdown()

	// Verify initial state
	s.mu.RLock()
	if len(s.listeners) != 1 {
		t.Fatalf("Expected 1 listener initially, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Update config to remove socat route
	newCfg := &config.Config{
		Verbose: false,
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: map[int]config.TCPRouteConfig{},
				Socat:  map[int]config.SocatRouteConfig{},
			},
		},
	}
	s.UpdateConfig(newCfg)

	// Verify socat listener was removed
	s.mu.RLock()
	if len(s.listeners) != 0 {
		t.Errorf("Expected 0 listeners after removing socat, got %d", len(s.listeners))
	}
	s.mu.RUnlock()

	// Verify port is released
	_, err = net.DialTimeout("tcp", "127.0.0.1:19800", 100*time.Millisecond)
	if err == nil {
		t.Error("Expected connection to removed socat port 19800 to fail")
	}
}

func TestServer_UpdateConfig_VerboseFlag(t *testing.T) {
	cfg := testConfig(map[int]config.TCPRouteConfig{
		19900: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
	})
	cfg.Verbose = false
	mgr := &mockManager{}
	s := NewServer(cfg, mgr)

	err := s.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer s.Shutdown()

	if s.verbose {
		t.Error("Expected verbose to be false initially")
	}

	// Update config with verbose enabled
	newCfg := testConfig(map[int]config.TCPRouteConfig{
		19900: {Context: "test", Namespace: "ns", Service: "svc", Port: 80},
	})
	newCfg.Verbose = true
	s.UpdateConfig(newCfg)

	if !s.verbose {
		t.Error("Expected verbose to be true after update")
	}
}
