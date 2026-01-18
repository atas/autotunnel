package tunnelmgr

import (
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/tunnel"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// testConfigWithTCP creates a test config with both HTTP and TCP routes
func testConfigWithTCP(httpRoutes map[string]config.K8sRouteConfig, tcpRoutes map[int]config.TCPRouteConfig) *config.Config {
	return &config.Config{
		HTTP: config.HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: config.K8sConfig{
				Routes:              httpRoutes,
				ResolvedKubeconfigs: []string{"/fake/kubeconfig"},
			},
		},
		TCP: config.TCPConfig{
			K8s: config.TCPK8sConfig{
				Routes: tcpRoutes,
			},
		},
	}
}

func TestGetOrCreateTCPTunnel_UnknownPort(t *testing.T) {
	cfg := testConfigWithTCP(nil, map[int]config.TCPRouteConfig{})
	m := NewManager(cfg)

	_, err := m.GetOrCreateTCPTunnel(9999)

	if err == nil {
		t.Error("Expected error for unknown port")
	}
	expectedErr := "no TCP route configured for port 9999"
	if err.Error() != expectedErr {
		t.Errorf("Expected error %q, got %q", expectedErr, err.Error())
	}
}

func TestGetOrCreateTCPTunnel_ReturnsExistingRunning(t *testing.T) {
	tcpRoutes := map[int]config.TCPRouteConfig{
		5432: {Context: "test", Namespace: "default", Service: "postgres", Port: 5432},
	}
	cfg := testConfigWithTCP(nil, tcpRoutes)
	m := NewManager(cfg)

	// Inject a running mock tunnel
	existingTunnel := newMockTunnel(true)
	m.tcpTunnels[5432] = existingTunnel

	// Act
	result, err := m.GetOrCreateTCPTunnel(5432)

	// Assert
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result != existingTunnel {
		t.Error("Expected to return the existing tunnel")
	}
	if !existingTunnel.wasTouched() {
		t.Error("Expected Touch() to be called on existing tunnel")
	}
}

func TestGetOrCreateTCPTunnel_RemovesFailedTunnel(t *testing.T) {
	tcpRoutes := map[int]config.TCPRouteConfig{
		5432: {Context: "test", Namespace: "default", Service: "postgres", Port: 5432},
	}
	cfg := testConfigWithTCP(nil, tcpRoutes)
	// Need HTTP kubeconfigs for fallback
	cfg.HTTP.K8s.ResolvedKubeconfigs = []string{"/fake/kubeconfig"}
	m := NewManager(cfg)

	// Inject a failed mock tunnel (should be replaced)
	failedTunnel := newMockTunnel(false)
	failedTunnel.state = tunnel.StateFailed // Failed state should trigger replacement
	m.tcpTunnels[5432] = failedTunnel

	// Set up factory to return a new mock
	factoryCalled := false
	newTunnel := newMockTunnel(false)
	m.tunnelFactory = func(hostname string, cfg config.K8sRouteConfig,
		clientset kubernetes.Interface, restConfig *rest.Config,
		listenAddr string, verbose bool) TunnelHandle {
		factoryCalled = true
		return newTunnel
	}

	// Pre-populate k8s client cache
	m.ClientFactory().InjectClient("test", nil, nil)

	// Act
	result, err := m.GetOrCreateTCPTunnel(5432)

	// Assert
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !factoryCalled {
		t.Error("Expected factory to be called for new tunnel")
	}
	if result != newTunnel {
		t.Error("Expected to return a new tunnel, not the stopped one")
	}
	if _, exists := m.tcpTunnels[5432]; !exists {
		t.Error("Expected new tunnel to be stored in map")
	}
}

func TestGetOrCreateTCPTunnel_FallsBackToHTTPKubeconfig(t *testing.T) {
	tcpRoutes := map[int]config.TCPRouteConfig{
		5432: {Context: "test", Namespace: "default", Service: "postgres", Port: 5432},
	}
	cfg := testConfigWithTCP(nil, tcpRoutes)
	// TCP has no kubeconfig, should fall back to HTTP
	cfg.TCP.K8s.ResolvedKubeconfigs = nil
	cfg.HTTP.K8s.ResolvedKubeconfigs = []string{"/http/kubeconfig"}
	m := NewManager(cfg)

	// Set up factory to verify it gets called
	factoryCalled := false
	newTunnel := newMockTunnel(false)
	m.tunnelFactory = func(hostname string, cfg config.K8sRouteConfig,
		clientset kubernetes.Interface, restConfig *rest.Config,
		listenAddr string, verbose bool) TunnelHandle {
		factoryCalled = true
		return newTunnel
	}

	// Pre-populate k8s client cache
	m.ClientFactory().InjectClient("test", nil, nil)

	// Act
	_, err := m.GetOrCreateTCPTunnel(5432)

	// Assert - if we got here without error, fallback worked
	if err != nil {
		t.Fatalf("Unexpected error (fallback may have failed): %v", err)
	}
	if !factoryCalled {
		t.Error("Expected factory to be called")
	}
}

func TestTCPRouteConfig_TargetName(t *testing.T) {
	tests := []struct {
		name     string
		route    config.TCPRouteConfig
		expected string
	}{
		{
			name:     "returns service name",
			route:    config.TCPRouteConfig{Service: "postgres"},
			expected: "postgres",
		},
		{
			name:     "returns pod name when no service",
			route:    config.TCPRouteConfig{Pod: "my-pod"},
			expected: "my-pod",
		},
		{
			name:     "prefers service over pod",
			route:    config.TCPRouteConfig{Service: "svc", Pod: "pod"},
			expected: "svc",
		},
		{
			name:     "returns empty when neither set",
			route:    config.TCPRouteConfig{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.route.TargetName()
			if result != tt.expected {
				t.Errorf("TargetName() = %q, want %q", result, tt.expected)
			}
		})
	}
}
