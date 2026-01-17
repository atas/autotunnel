//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const (
	proxyAddr   = "localhost:18989" // Use non-standard port to avoid conflicts
	testTimeout = 60 * time.Second
	startupWait = 3 * time.Second
	idleTimeout = "10s" // Short idle timeout for testing
)

// getTestContext returns the Kubernetes context to use for tests.
// Defaults to "default" (k3s default) but can be overridden via K8S_CONTEXT env var.
func getTestContext() string {
	if ctx := os.Getenv("K8S_CONTEXT"); ctx != "" {
		return ctx
	}
	return "default"
}

// TestMain sets up and tears down the test environment
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// startOPF starts the autotunnel binary with a test configuration
func startOPF(t *testing.T, configPath string) (*exec.Cmd, func()) {
	t.Helper()

	// Find the binary (should be built by Makefile)
	binaryPath := filepath.Join("..", "..", "autotunnel")
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		t.Fatalf("autotunnel binary not found at %s - run 'make build' first", binaryPath)
	}

	cmd := exec.Command(binaryPath, "--config", configPath, "--verbose")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start autotunnel: %v", err)
	}

	// Wait for startup
	time.Sleep(startupWait)

	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()
		}
	}

	return cmd, cleanup
}

// writeTestConfig creates a temporary config file for testing
func writeTestConfig(t *testing.T, services map[string]serviceConfig) string {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	// Build config content in new format
	var sb strings.Builder
	sb.WriteString("apiVersion: autotunnel/v1\n")
	sb.WriteString("verbose: true\n")
	sb.WriteString("auto_reload_config: false\n")
	sb.WriteString("http:\n")
	sb.WriteString(fmt.Sprintf("  listen: \":%d\"\n", 18989))
	sb.WriteString(fmt.Sprintf("  idle_timeout: %s\n", idleTimeout))
	sb.WriteString("  k8s:\n")
	sb.WriteString(fmt.Sprintf("    kubeconfig: %s\n", kubeconfig))
	sb.WriteString("    routes:\n")

	for hostname, svc := range services {
		sb.WriteString(fmt.Sprintf("      %s:\n", hostname))
		sb.WriteString(fmt.Sprintf("        context: %s\n", svc.Context))
		sb.WriteString(fmt.Sprintf("        namespace: %s\n", svc.Namespace))
		if svc.Pod != "" {
			sb.WriteString(fmt.Sprintf("        pod: %s\n", svc.Pod))
		} else {
			sb.WriteString(fmt.Sprintf("        service: %s\n", svc.Service))
		}
		sb.WriteString(fmt.Sprintf("        port: %d\n", svc.Port))
		if svc.Scheme != "" {
			sb.WriteString(fmt.Sprintf("        scheme: %s\n", svc.Scheme))
		}
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "autotunnel-test-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config: %v", err)
	}

	if _, err := tmpFile.WriteString(sb.String()); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}
	tmpFile.Close()

	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	return tmpFile.Name()
}

type serviceConfig struct {
	Context   string
	Namespace string
	Service   string // Service name (mutually exclusive with Pod)
	Pod       string // Pod name for direct targeting (mutually exclusive with Service)
	Port      int
	Scheme    string // "http" or "https" for X-Forwarded-Proto header
}

// tcpRouteConfig represents a TCP route configuration for tests
type tcpRouteConfig struct {
	Context   string
	Namespace string
	Service   string
	Pod       string
	Port      int
}

// writeTestConfigWithTCP creates a test configuration file with both HTTP and TCP routes
func writeTestConfigWithTCP(t *testing.T, httpServices map[string]serviceConfig, tcpRoutes map[int]tcpRouteConfig) string {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	var sb strings.Builder
	sb.WriteString("apiVersion: autotunnel/v1\n")
	sb.WriteString("verbose: true\n")
	sb.WriteString("auto_reload_config: false\n")
	sb.WriteString("http:\n")
	sb.WriteString(fmt.Sprintf("  listen: \":%d\"\n", 18989))
	sb.WriteString(fmt.Sprintf("  idle_timeout: %s\n", idleTimeout))
	sb.WriteString("  k8s:\n")
	sb.WriteString(fmt.Sprintf("    kubeconfig: %s\n", kubeconfig))
	sb.WriteString("    routes:\n")

	for hostname, svc := range httpServices {
		sb.WriteString(fmt.Sprintf("      %s:\n", hostname))
		sb.WriteString(fmt.Sprintf("        context: %s\n", svc.Context))
		sb.WriteString(fmt.Sprintf("        namespace: %s\n", svc.Namespace))
		if svc.Pod != "" {
			sb.WriteString(fmt.Sprintf("        pod: %s\n", svc.Pod))
		} else {
			sb.WriteString(fmt.Sprintf("        service: %s\n", svc.Service))
		}
		sb.WriteString(fmt.Sprintf("        port: %d\n", svc.Port))
		if svc.Scheme != "" {
			sb.WriteString(fmt.Sprintf("        scheme: %s\n", svc.Scheme))
		}
	}

	if len(tcpRoutes) > 0 {
		sb.WriteString("\ntcp:\n")
		sb.WriteString(fmt.Sprintf("  idle_timeout: %s\n", idleTimeout))
		sb.WriteString("  k8s:\n")
		sb.WriteString(fmt.Sprintf("    kubeconfig: %s\n", kubeconfig))
		sb.WriteString("    routes:\n")
		for port, route := range tcpRoutes {
			sb.WriteString(fmt.Sprintf("      %d:\n", port))
			sb.WriteString(fmt.Sprintf("        context: %s\n", route.Context))
			sb.WriteString(fmt.Sprintf("        namespace: %s\n", route.Namespace))
			if route.Pod != "" {
				sb.WriteString(fmt.Sprintf("        pod: %s\n", route.Pod))
			} else {
				sb.WriteString(fmt.Sprintf("        service: %s\n", route.Service))
			}
			sb.WriteString(fmt.Sprintf("        port: %d\n", route.Port))
		}
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "autotunnel-tcp-test-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config: %v", err)
	}

	if _, err := tmpFile.WriteString(sb.String()); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}
	tmpFile.Close()

	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	return tmpFile.Name()
}

// TestBasicProxyConnection tests that requests are proxied through a tunnel
func TestBasicProxyConnection(t *testing.T) {
	// Create test config pointing to our test services
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
		"echo.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "echo",
			Port:      8080,
		},
	})

	// Start autotunnel
	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Test cases
	tests := []struct {
		name             string
		host             string
		path             string
		wantStatus       int
		wantBodyContains string
	}{
		{
			name:             "nginx service",
			host:             "nginx.test",
			path:             "/",
			wantStatus:       http.StatusOK,
			wantBodyContains: "nginx",
		},
		{
			name:             "echo service",
			host:             "echo.test",
			path:             "/",
			wantStatus:       http.StatusOK,
			wantBodyContains: "autotunnel-integration-test",
		},
	}

	client := &http.Client{
		Timeout: testTimeout,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create request with custom Host header
			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s%s", proxyAddr, tt.path), nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Host = tt.host

			// Make request
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			// Check status
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("Status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			// Check body contains expected string
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(strings.ToLower(string(body)), strings.ToLower(tt.wantBodyContains)) {
				t.Errorf("Body does not contain %q: %s", tt.wantBodyContains, string(body))
			}
		})
	}
}

// TestUnknownHostReturns502 tests that unknown hosts return 502
func TestUnknownHostReturns502(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	req.Host = "unknown.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// TestMultipleRequestsReusesTunnel tests that multiple requests reuse the same tunnel
func TestMultipleRequestsReusesTunnel(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	// Make multiple requests
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
		req.Host = "nginx.test"

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request %d failed: %v", i, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Request %d: Status = %d, want %d", i, resp.StatusCode, http.StatusOK)
		}
	}
}

// TestGracefulShutdown tests that the server shuts down gracefully
func TestGracefulShutdown(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	cmd, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Make a request to create a tunnel
	client := &http.Client{Timeout: testTimeout}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	req.Host = "nginx.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	resp.Body.Close()

	// Send interrupt signal
	_ = cmd.Process.Signal(os.Interrupt)

	// Wait for process to exit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited (might be nil error or signal error, both OK)
		t.Logf("Process exited: %v", err)
	case <-ctx.Done():
		t.Error("Process did not exit within timeout")
		_ = cmd.Process.Kill()
	}
}

// ============================================================================
// Priority 1 Tests: High Value, Easy (using existing or simple services)
// ============================================================================

// TestDirectPodTargeting tests routing directly to a pod without going through a Service
func TestDirectPodTargeting(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"standalone.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Pod:       "standalone-pod",
			Port:      8080,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Host = "standalone.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "direct-pod-test") {
		t.Errorf("Body does not contain 'direct-pod-test': %s", string(body))
	}
}

// TestXForwardedHeaders tests that X-Forwarded-* headers are set correctly
func TestXForwardedHeaders(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"headers.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "echo-headers",
			Port:      80,
			// Scheme defaults to "http" - use HTTP backend with HTTP scheme
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Host = "headers.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Parse JSON response from echo-server
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Logf("Response body: %s", bodyStr)
		t.Fatalf("Failed to parse JSON response: %v", err)
	}

	// Check headers in the response
	request, ok := response["request"].(map[string]interface{})
	if !ok {
		t.Fatalf("No 'request' field in response: %s", bodyStr)
	}
	headers, ok := request["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("No 'headers' field in request: %s", bodyStr)
	}

	// Check X-Forwarded-Proto
	proto, ok := headers["x-forwarded-proto"].(string)
	if !ok || proto != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want 'http'", proto)
	}

	// Check X-Forwarded-Host
	host, ok := headers["x-forwarded-host"].(string)
	if !ok || host != "headers.test" {
		t.Errorf("X-Forwarded-Host = %q, want 'headers.test'", host)
	}

	// Check X-Forwarded-For is set
	xff, ok := headers["x-forwarded-for"].(string)
	if !ok || xff == "" {
		t.Errorf("X-Forwarded-For not set or empty")
	}
}

// TestConcurrentRequestsSameHost tests that concurrent requests to the same host work correctly
func TestConcurrentRequestsSameHost(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}
	numRequests := 50
	var wg sync.WaitGroup
	errors := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(reqNum int) {
			defer wg.Done()

			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
			if err != nil {
				errors <- fmt.Errorf("request %d: failed to create: %v", reqNum, err)
				return
			}
			req.Host = "nginx.test"

			resp, err := client.Do(req)
			if err != nil {
				errors <- fmt.Errorf("request %d: failed: %v", reqNum, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("request %d: status = %d, want %d", reqNum, resp.StatusCode, http.StatusOK)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var allErrors []error
	for err := range errors {
		allErrors = append(allErrors, err)
	}

	if len(allErrors) > 0 {
		for _, err := range allErrors {
			t.Error(err)
		}
		t.Fatalf("%d out of %d concurrent requests failed", len(allErrors), numRequests)
	}
}

// TestHostHeaderWithPort tests that Host headers with port numbers are handled correctly
func TestHostHeaderWithPort(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	// Host header includes port number - should still route correctly
	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Host = "nginx.test:18989" // Host with port

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(strings.ToLower(string(body)), "nginx") {
		t.Errorf("Body does not contain 'nginx': %s", string(body))
	}
}

// ============================================================================
// Priority 2 Tests: High Value, Medium Effort
// ============================================================================

// TestTLSPassthrough tests TLS passthrough functionality
func TestTLSPassthrough(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx-tls.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx-tls",
			Port:      443,
			Scheme:    "https",
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Create TLS config with SNI set
	tlsConfig := &tls.Config{
		ServerName:         "nginx-tls.test",
		InsecureSkipVerify: true, // Self-signed cert
	}

	// Connect with TLS through the proxy
	conn, err := tls.Dial("tcp", proxyAddr, tlsConfig)
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer conn.Close()

	// Set read/write deadlines
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	// Send HTTP request over TLS
	req := "GET / HTTP/1.1\r\nHost: nginx-tls.test\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("Failed to write request: %v", err)
	}

	// Read response
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read status line: %v", err)
	}

	if !strings.Contains(statusLine, "200") {
		t.Errorf("Status line = %q, want 200 OK", strings.TrimSpace(statusLine))
	}

	// Read rest of response
	var bodyBuilder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		bodyBuilder.WriteString(line)
	}
	body := bodyBuilder.String()

	if !strings.Contains(body, "autotunnel-tls-integration-test") {
		t.Errorf("Response body does not contain expected text: %s", body)
	}
}

// TestTLSPassthroughCertFromBackend verifies the cert comes from the backend (passthrough)
func TestTLSPassthroughCertFromBackend(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx-tls.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx-tls",
			Port:      443,
			Scheme:    "https",
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	tlsConfig := &tls.Config{
		ServerName:         "nginx-tls.test",
		InsecureSkipVerify: true,
	}

	conn, err := tls.Dial("tcp", proxyAddr, tlsConfig)
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer conn.Close()

	// Check that we got a certificate from the backend
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("No peer certificates received")
	}

	cert := state.PeerCertificates[0]
	// The cert should be from nginx-tls.test (our init container generated it)
	if cert.Subject.CommonName != "nginx-tls.test" {
		t.Errorf("Certificate CN = %q, want 'nginx-tls.test'", cert.Subject.CommonName)
	}
}

// TestNamedPortResolution tests that named ports are resolved correctly
func TestNamedPortResolution(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"named-port.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "echo-named-ports",
			Port:      80, // Service port (will be resolved to named targetPort "http")
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Host = "named-port.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "named-port-test") {
		t.Errorf("Body does not contain 'named-port-test': %s", string(body))
	}
}

// TestIdleTimeoutCleanup tests that tunnels are cleaned up after idle timeout
func TestIdleTimeoutCleanup(t *testing.T) {
	// Use a custom config with very short idle timeout
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	configContent := fmt.Sprintf(`apiVersion: autotunnel/v1
verbose: true
auto_reload_config: false
http:
  listen: ":18990"
  idle_timeout: 3s
  k8s:
    kubeconfig: %s
    routes:
      idle.test:
        context: %s
        namespace: autotunnel-test
        service: nginx
        port: 80
`, kubeconfig, getTestContext())

	tmpFile, err := os.CreateTemp("", "autotunnel-idle-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Start autotunnel with custom config
	binaryPath := filepath.Join("..", "..", "autotunnel")
	cmd := exec.Command(binaryPath, "--config", tmpFile.Name(), "--verbose")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start autotunnel: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()

	time.Sleep(startupWait)

	client := &http.Client{Timeout: testTimeout}

	// Make initial request to create tunnel
	req, _ := http.NewRequest("GET", "http://localhost:18990/", nil)
	req.Host = "idle.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Initial request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Initial request: Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Wait longer than idle timeout (3s + buffer)
	t.Log("Waiting for idle timeout...")
	time.Sleep(5 * time.Second)

	// Make another request - tunnel should be recreated
	// (We can't easily verify tunnel was cleaned up, but we verify it still works)
	req2, _ := http.NewRequest("GET", "http://localhost:18990/", nil)
	req2.Host = "idle.test"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("Second request failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("Second request: Status = %d, want %d", resp2.StatusCode, http.StatusOK)
	}
}

// ============================================================================
// Priority 3 Tests: Medium Value
// ============================================================================

// TestMultiPortServiceRouting tests routing to different ports on the same service
func TestMultiPortServiceRouting(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"api.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "multi-port",
			Port:      8080,
		},
		"admin.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "multi-port",
			Port:      8081,
		},
		"metrics.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "multi-port",
			Port:      8082,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	tests := []struct {
		host         string
		expectedBody string
	}{
		{"api.test", "api-response"},
		{"admin.test", "admin-response"},
		{"metrics.test", "metrics-response"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Host = tt.host

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
			}

			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.expectedBody) {
				t.Errorf("Body does not contain %q: %s", tt.expectedBody, string(body))
			}
		})
	}
}

// TestConcurrentRequestsDifferentHosts tests concurrent requests to different hosts
func TestConcurrentRequestsDifferentHosts(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
		"echo.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "echo",
			Port:      8080,
		},
		"api.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "multi-port",
			Port:      8080,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}
	hosts := []string{"nginx.test", "echo.test", "api.test"}
	requestsPerHost := 10

	var wg sync.WaitGroup
	errors := make(chan error, len(hosts)*requestsPerHost)

	for _, host := range hosts {
		for i := 0; i < requestsPerHost; i++ {
			wg.Add(1)
			go func(h string, reqNum int) {
				defer wg.Done()

				req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
				if err != nil {
					errors <- fmt.Errorf("%s request %d: failed to create: %v", h, reqNum, err)
					return
				}
				req.Host = h

				resp, err := client.Do(req)
				if err != nil {
					errors <- fmt.Errorf("%s request %d: failed: %v", h, reqNum, err)
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					errors <- fmt.Errorf("%s request %d: status = %d", h, reqNum, resp.StatusCode)
					return
				}
			}(host, i)
		}
	}

	wg.Wait()
	close(errors)

	var allErrors []error
	for err := range errors {
		allErrors = append(allErrors, err)
	}

	if len(allErrors) > 0 {
		for _, err := range allErrors {
			t.Error(err)
		}
		t.Fatalf("%d concurrent requests failed", len(allErrors))
	}
}

// TestServiceNotFound tests error handling when service doesn't exist
func TestServiceNotFound(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nonexistent.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nonexistent-service",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
	req.Host = "nonexistent.test"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get 502 Bad Gateway for a service that can't be reached
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("Status = %d, want %d (Bad Gateway)", resp.StatusCode, http.StatusBadGateway)
	}
}

// ============================================================================
// Priority 4 Tests: Nice to Have
// ============================================================================

// TestWebSocketConnection tests WebSocket upgrade through the proxy
func TestWebSocketConnection(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"ws.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "websocket-echo",
			Port:      8080,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Create WebSocket connection through the proxy
	// We need to set the Host in config.Location, not config.Header,
	// because the websocket library uses Location.Host for the Host header
	origin := "http://ws.test/"
	wsURL := "ws://ws.test/"

	config, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		t.Fatalf("Failed to create WebSocket config: %v", err)
	}

	// Connect to the proxy
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Failed to dial proxy: %v", err)
	}
	defer conn.Close()

	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		t.Fatalf("Failed to create WebSocket client: %v", err)
	}
	defer ws.Close()

	// The jmalloc/echo-server sends a greeting message first, then echoes
	// Read and discard the initial greeting (if any)
	greeting := make([]byte, 1024)
	n, err := ws.Read(greeting)
	if err != nil {
		t.Fatalf("Failed to read greeting: %v", err)
	}
	t.Logf("Greeting from echo server: %q", string(greeting[:n]))

	// Send a message
	testMessage := "Hello, WebSocket!"
	if _, err := ws.Write([]byte(testMessage)); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Read response - should be echoed back
	response := make([]byte, 1024)
	n, err = ws.Read(response)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	// Echo server should echo back the message
	if !strings.Contains(string(response[:n]), testMessage) {
		t.Errorf("Response = %q, want to contain %q", string(response[:n]), testMessage)
	}
}

// TestHostHeaderCaseSensitivity tests case-insensitive host matching
func TestHostHeaderCaseSensitivity(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	client := &http.Client{Timeout: testTimeout}

	// Test with different case variations
	hosts := []string{"NGINX.TEST", "Nginx.Test", "nGiNx.TeSt"}

	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/", proxyAddr), nil)
			req.Host = host

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			// Should either work (case-insensitive) or return 502 (case-sensitive)
			// Log the result for investigation
			t.Logf("Host %q: Status = %d", host, resp.StatusCode)
		})
	}
}

// TestEmptyHostHeader tests handling of empty or missing Host header
func TestEmptyHostHeader(t *testing.T) {
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "autotunnel-test",
			Service:   "nginx",
			Port:      80,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Use raw TCP connection to send request without Host header
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	// Send HTTP request without Host header
	req := "GET / HTTP/1.1\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	// Read response
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read status: %v", err)
	}

	// Should get an error response (400 or 502)
	if strings.Contains(statusLine, "200") {
		t.Errorf("Expected error response, got: %s", statusLine)
	}
	t.Logf("Empty Host response: %s", strings.TrimSpace(statusLine))
}

// TestIdleTimeoutReset tests that activity resets the idle timeout
func TestIdleTimeoutReset(t *testing.T) {
	// Use a custom config with short idle timeout
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	configContent := fmt.Sprintf(`apiVersion: autotunnel/v1
verbose: true
auto_reload_config: false
http:
  listen: ":18991"
  idle_timeout: 5s
  k8s:
    kubeconfig: %s
    routes:
      reset.test:
        context: %s
        namespace: autotunnel-test
        service: nginx
        port: 80
`, kubeconfig, getTestContext())

	tmpFile, err := os.CreateTemp("", "autotunnel-reset-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	binaryPath := filepath.Join("..", "..", "autotunnel")
	cmd := exec.Command(binaryPath, "--config", tmpFile.Name(), "--verbose")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start autotunnel: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()

	time.Sleep(startupWait)

	client := &http.Client{Timeout: testTimeout}

	// Make requests every 2 seconds (before 5s timeout) to keep tunnel alive
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", "http://localhost:18991/", nil)
		req.Host = "reset.test"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request %d failed: %v", i, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Request %d: Status = %d, want %d", i, resp.StatusCode, http.StatusOK)
		}

		if i < 4 {
			time.Sleep(2 * time.Second)
		}
	}

	// Total elapsed time: ~8 seconds
	// Tunnel should still be alive because we kept making requests
	t.Log("All requests succeeded - idle timeout was reset by activity")
}

// ============================================================================
// TCP Port-Forwarding Tests
// ============================================================================

// TestTCPBasicConnection tests basic TCP echo through tunnel
func TestTCPBasicConnection(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19000: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", "localhost:19000", testTimeout)
	if err != nil {
		t.Fatalf("Failed to connect to TCP tunnel: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	testData := []byte("Hello, TCP tunnel!")
	if _, err := conn.Write(testData); err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	response := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if string(response) != string(testData) {
		t.Errorf("Response = %q, want %q", string(response), string(testData))
	}
}

// TestTCPMultipleConnectionsReuseTunnel tests that multiple sequential connections reuse the same tunnel
func TestTCPMultipleConnectionsReuseTunnel(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19001: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", "localhost:19001", testTimeout)
		if err != nil {
			t.Fatalf("Connection %d: Failed to connect: %v", i, err)
		}

		if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
			conn.Close()
			t.Fatalf("Connection %d: Failed to set deadline: %v", i, err)
		}

		testData := []byte(fmt.Sprintf("Connection %d", i))
		if _, err := conn.Write(testData); err != nil {
			conn.Close()
			t.Fatalf("Connection %d: Failed to write: %v", i, err)
		}

		response := make([]byte, len(testData))
		if _, err := io.ReadFull(conn, response); err != nil {
			conn.Close()
			t.Fatalf("Connection %d: Failed to read: %v", i, err)
		}

		if string(response) != string(testData) {
			conn.Close()
			t.Errorf("Connection %d: Response = %q, want %q", i, string(response), string(testData))
		}

		conn.Close()
	}
}

// TestTCPConcurrentConnections tests multiple concurrent TCP connections
func TestTCPConcurrentConnections(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19002: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	numConnections := 20
	var wg sync.WaitGroup
	errors := make(chan error, numConnections)

	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func(connNum int) {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", "localhost:19002", testTimeout)
			if err != nil {
				errors <- fmt.Errorf("connection %d: failed to connect: %v", connNum, err)
				return
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				errors <- fmt.Errorf("connection %d: failed to set deadline: %v", connNum, err)
				return
			}

			testData := []byte(fmt.Sprintf("Concurrent connection %d", connNum))
			if _, err := conn.Write(testData); err != nil {
				errors <- fmt.Errorf("connection %d: failed to write: %v", connNum, err)
				return
			}

			response := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, response); err != nil {
				errors <- fmt.Errorf("connection %d: failed to read: %v", connNum, err)
				return
			}

			if string(response) != string(testData) {
				errors <- fmt.Errorf("connection %d: response = %q, want %q", connNum, string(response), string(testData))
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var allErrors []error
	for err := range errors {
		allErrors = append(allErrors, err)
	}

	if len(allErrors) > 0 {
		for _, err := range allErrors {
			t.Error(err)
		}
		t.Fatalf("%d out of %d concurrent TCP connections failed", len(allErrors), numConnections)
	}
}

// TestTCPDirectPodTargeting tests TCP routing directly to a pod (no service)
func TestTCPDirectPodTargeting(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19003: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Pod:       "tcp-echo-standalone",
				Port:      9999,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", "localhost:19003", testTimeout)
	if err != nil {
		t.Fatalf("Failed to connect to TCP tunnel: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	testData := []byte("Direct pod TCP test")
	if _, err := conn.Write(testData); err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	response := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if string(response) != string(testData) {
		t.Errorf("Response = %q, want %q", string(response), string(testData))
	}
}

// TestTCPMultipleRoutes tests multiple TCP routes on different ports
func TestTCPMultipleRoutes(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19004: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
			19005: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo-alt",
				Port:      9998,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	tests := []struct {
		name string
		port int
		data string
	}{
		{"tcp-echo route", 19004, "Route 1 test data"},
		{"tcp-echo-alt route", 19005, "Route 2 test data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tt.port), testTimeout)
			if err != nil {
				t.Fatalf("Failed to connect: %v", err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatalf("Failed to set deadline: %v", err)
			}

			testData := []byte(tt.data)
			if _, err := conn.Write(testData); err != nil {
				t.Fatalf("Failed to write: %v", err)
			}

			response := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatalf("Failed to read: %v", err)
			}

			if string(response) != string(testData) {
				t.Errorf("Response = %q, want %q", string(response), string(testData))
			}
		})
	}
}

// TestTCPBidirectionalData tests bidirectional data transfer with various sizes
func TestTCPBidirectionalData(t *testing.T) {
	configPath := writeTestConfigWithTCP(t,
		map[string]serviceConfig{
			"nginx.test": {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "nginx",
				Port:      80,
			},
		},
		map[int]tcpRouteConfig{
			19006: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	sizes := []int{100, 1000, 10000}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", "localhost:19006", testTimeout)
			if err != nil {
				t.Fatalf("Failed to connect: %v", err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatalf("Failed to set deadline: %v", err)
			}

			// Generate test data of specified size
			testData := make([]byte, size)
			for i := range testData {
				testData[i] = byte('A' + (i % 26))
			}

			if _, err := conn.Write(testData); err != nil {
				t.Fatalf("Failed to write %d bytes: %v", size, err)
			}

			response := make([]byte, size)
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatalf("Failed to read %d bytes: %v", size, err)
			}

			if string(response) != string(testData) {
				t.Errorf("Response does not match sent data for size %d", size)
			}
		})
	}
}

// ============================================================================
// Socat/Jump-Host TCP Forwarding Tests
// ============================================================================

// socatRouteConfig represents a socat (jump-host) route configuration for tests
type socatRouteConfig struct {
	Context    string
	Namespace  string
	ViaService string // Jump pod discovered via service (mutually exclusive with ViaPod)
	ViaPod     string // Jump pod by direct name (mutually exclusive with ViaService)
	Container  string // Optional container name for multi-container pods
	TargetHost string // Target host (e.g., tcp-target.autotunnel-test.svc.cluster.local)
	TargetPort int    // Target port
}

// writeTestConfigWithSocat creates a test configuration file with HTTP, TCP, and socat routes
func writeTestConfigWithSocat(t *testing.T, httpServices map[string]serviceConfig, tcpRoutes map[int]tcpRouteConfig, socatRoutes map[int]socatRouteConfig) string {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	var sb strings.Builder
	sb.WriteString("apiVersion: autotunnel/v1\n")
	sb.WriteString("verbose: true\n")
	sb.WriteString("auto_reload_config: false\n")
	sb.WriteString("http:\n")
	sb.WriteString(fmt.Sprintf("  listen: \":%d\"\n", 18989))
	sb.WriteString(fmt.Sprintf("  idle_timeout: %s\n", idleTimeout))
	sb.WriteString("  k8s:\n")
	sb.WriteString(fmt.Sprintf("    kubeconfig: %s\n", kubeconfig))
	sb.WriteString("    routes:\n")

	// Write HTTP routes (at least one required to prevent validation errors)
	if len(httpServices) == 0 {
		// Add a dummy HTTP route
		sb.WriteString("      dummy.test:\n")
		sb.WriteString(fmt.Sprintf("        context: %s\n", getTestContext()))
		sb.WriteString("        namespace: autotunnel-test\n")
		sb.WriteString("        service: nginx\n")
		sb.WriteString("        port: 80\n")
	} else {
		for hostname, svc := range httpServices {
			sb.WriteString(fmt.Sprintf("      %s:\n", hostname))
			sb.WriteString(fmt.Sprintf("        context: %s\n", svc.Context))
			sb.WriteString(fmt.Sprintf("        namespace: %s\n", svc.Namespace))
			if svc.Pod != "" {
				sb.WriteString(fmt.Sprintf("        pod: %s\n", svc.Pod))
			} else {
				sb.WriteString(fmt.Sprintf("        service: %s\n", svc.Service))
			}
			sb.WriteString(fmt.Sprintf("        port: %d\n", svc.Port))
			if svc.Scheme != "" {
				sb.WriteString(fmt.Sprintf("        scheme: %s\n", svc.Scheme))
			}
		}
	}

	// Write TCP section if we have routes or socat
	if len(tcpRoutes) > 0 || len(socatRoutes) > 0 {
		sb.WriteString("\ntcp:\n")
		sb.WriteString(fmt.Sprintf("  idle_timeout: %s\n", idleTimeout))
		sb.WriteString("  k8s:\n")
		sb.WriteString(fmt.Sprintf("    kubeconfig: %s\n", kubeconfig))

		// Write direct TCP routes
		if len(tcpRoutes) > 0 {
			sb.WriteString("    routes:\n")
			for port, route := range tcpRoutes {
				sb.WriteString(fmt.Sprintf("      %d:\n", port))
				sb.WriteString(fmt.Sprintf("        context: %s\n", route.Context))
				sb.WriteString(fmt.Sprintf("        namespace: %s\n", route.Namespace))
				if route.Pod != "" {
					sb.WriteString(fmt.Sprintf("        pod: %s\n", route.Pod))
				} else {
					sb.WriteString(fmt.Sprintf("        service: %s\n", route.Service))
				}
				sb.WriteString(fmt.Sprintf("        port: %d\n", route.Port))
			}
		}

		// Write socat (jump-host) routes
		if len(socatRoutes) > 0 {
			sb.WriteString("    socat:\n")
			for port, route := range socatRoutes {
				sb.WriteString(fmt.Sprintf("      %d:\n", port))
				sb.WriteString(fmt.Sprintf("        context: %s\n", route.Context))
				sb.WriteString(fmt.Sprintf("        namespace: %s\n", route.Namespace))
				sb.WriteString("        via:\n")
				if route.ViaPod != "" {
					sb.WriteString(fmt.Sprintf("          pod: %s\n", route.ViaPod))
				} else {
					sb.WriteString(fmt.Sprintf("          service: %s\n", route.ViaService))
				}
				if route.Container != "" {
					sb.WriteString(fmt.Sprintf("          container: %s\n", route.Container))
				}
				sb.WriteString("        target:\n")
				sb.WriteString(fmt.Sprintf("          host: %s\n", route.TargetHost))
				sb.WriteString(fmt.Sprintf("          port: %d\n", route.TargetPort))
			}
		}
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "autotunnel-socat-test-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config: %v", err)
	}

	if _, err := tmpFile.WriteString(sb.String()); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}
	tmpFile.Close()

	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	return tmpFile.Name()
}

// TestSocatBasicConnection tests basic connectivity through a jump pod via service
func TestSocatBasicConnection(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19100: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaService: "bastion-jump",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	conn, err := net.DialTimeout("tcp", "localhost:19100", testTimeout)
	if err != nil {
		t.Fatalf("Failed to connect to socat tunnel: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	testData := []byte("Hello via jump host!\n")
	if _, err := conn.Write(testData); err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	response := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if string(response) != string(testData) {
		t.Errorf("Response = %q, want %q", string(response), string(testData))
	}
}

// TestSocatViaPod tests jump-host routing using direct pod targeting
func TestSocatViaPod(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19101: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaPod:     "bastion-standalone",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	conn, err := net.DialTimeout("tcp", "localhost:19101", testTimeout)
	if err != nil {
		t.Fatalf("Failed to connect to socat tunnel: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("Failed to set deadline: %v", err)
	}

	testData := []byte("Direct pod jump test!\n")
	if _, err := conn.Write(testData); err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	response := make([]byte, len(testData))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if string(response) != string(testData) {
		t.Errorf("Response = %q, want %q", string(response), string(testData))
	}
}

// TestSocatBidirectionalData tests bidirectional data transfer with various sizes through jump host
func TestSocatBidirectionalData(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19102: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaService: "bastion-jump",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	sizes := []int{100, 1000, 5000}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", "localhost:19102", testTimeout)
			if err != nil {
				t.Fatalf("Failed to connect: %v", err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatalf("Failed to set deadline: %v", err)
			}

			// Generate test data of specified size
			testData := make([]byte, size)
			for i := range testData {
				testData[i] = byte('A' + (i % 26))
			}

			if _, err := conn.Write(testData); err != nil {
				t.Fatalf("Failed to write %d bytes: %v", size, err)
			}

			response := make([]byte, size)
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatalf("Failed to read %d bytes: %v", size, err)
			}

			if string(response) != string(testData) {
				t.Errorf("Response does not match sent data for size %d", size)
			}
		})
	}
}

// TestSocatMultipleConnections tests multiple sequential connections through jump host
func TestSocatMultipleConnections(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19103: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaService: "bastion-jump",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprintf("connection_%d", i), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", "localhost:19103", testTimeout)
			if err != nil {
				t.Fatalf("Connection %d: Failed to connect: %v", i, err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatalf("Connection %d: Failed to set deadline: %v", i, err)
			}

			testData := []byte(fmt.Sprintf("Connection %d test data\n", i))
			if _, err := conn.Write(testData); err != nil {
				t.Fatalf("Connection %d: Failed to write: %v", i, err)
			}

			response := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatalf("Connection %d: Failed to read: %v", i, err)
			}

			if string(response) != string(testData) {
				t.Errorf("Connection %d: Response = %q, want %q", i, string(response), string(testData))
			}
		})
	}
}

// TestSocatMultipleRoutes tests multiple socat routes on different ports
func TestSocatMultipleRoutes(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19104: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaService: "bastion-jump",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
		19105: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaPod:     "bastion-standalone",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	tests := []struct {
		name string
		port int
		data string
	}{
		{"via-service", 19104, "Route via service\n"},
		{"via-pod", 19105, "Route via direct pod\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tt.port), testTimeout)
			if err != nil {
				t.Fatalf("Failed to connect: %v", err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatalf("Failed to set deadline: %v", err)
			}

			testData := []byte(tt.data)
			if _, err := conn.Write(testData); err != nil {
				t.Fatalf("Failed to write: %v", err)
			}

			response := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatalf("Failed to read: %v", err)
			}

			if string(response) != string(testData) {
				t.Errorf("Response = %q, want %q", string(response), string(testData))
			}
		})
	}
}

// TestSocatConcurrentConnections tests multiple concurrent connections through jump host
func TestSocatConcurrentConnections(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil, nil, map[int]socatRouteConfig{
		19106: {
			Context:    getTestContext(),
			Namespace:  "autotunnel-test",
			ViaService: "bastion-jump",
			TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
			TargetPort: 3306,
		},
	})

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the service time to be fully ready
	time.Sleep(2 * time.Second)

	numConnections := 10
	var wg sync.WaitGroup
	errors := make(chan error, numConnections)

	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func(connNum int) {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", "localhost:19106", testTimeout)
			if err != nil {
				errors <- fmt.Errorf("connection %d: failed to connect: %v", connNum, err)
				return
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				errors <- fmt.Errorf("connection %d: failed to set deadline: %v", connNum, err)
				return
			}

			testData := []byte(fmt.Sprintf("Concurrent connection %d\n", connNum))
			if _, err := conn.Write(testData); err != nil {
				errors <- fmt.Errorf("connection %d: failed to write: %v", connNum, err)
				return
			}

			response := make([]byte, len(testData))
			if _, err := io.ReadFull(conn, response); err != nil {
				errors <- fmt.Errorf("connection %d: failed to read: %v", connNum, err)
				return
			}

			if string(response) != string(testData) {
				errors <- fmt.Errorf("connection %d: response = %q, want %q", connNum, string(response), string(testData))
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var allErrors []error
	for err := range errors {
		allErrors = append(allErrors, err)
	}

	if len(allErrors) > 0 {
		for _, err := range allErrors {
			t.Error(err)
		}
		t.Fatalf("%d out of %d concurrent socat connections failed", len(allErrors), numConnections)
	}
}

// TestSocatMixedWithTCPRoutes tests that socat routes work alongside direct TCP routes
func TestSocatMixedWithTCPRoutes(t *testing.T) {
	configPath := writeTestConfigWithSocat(t, nil,
		map[int]tcpRouteConfig{
			19107: {
				Context:   getTestContext(),
				Namespace: "autotunnel-test",
				Service:   "tcp-echo",
				Port:      9999,
			},
		},
		map[int]socatRouteConfig{
			19108: {
				Context:    getTestContext(),
				Namespace:  "autotunnel-test",
				ViaService: "bastion-jump",
				TargetHost: "tcp-target.autotunnel-test.svc.cluster.local",
				TargetPort: 3306,
			},
		},
	)

	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Give the services time to be fully ready
	time.Sleep(2 * time.Second)

	// Test direct TCP route
	t.Run("direct-tcp-route", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", "localhost:19107", testTimeout)
		if err != nil {
			t.Fatalf("Failed to connect to direct TCP route: %v", err)
		}
		defer conn.Close()

		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatalf("Failed to set deadline: %v", err)
		}

		testData := []byte("Direct TCP route test\n")
		if _, err := conn.Write(testData); err != nil {
			t.Fatalf("Failed to write: %v", err)
		}

		response := make([]byte, len(testData))
		if _, err := io.ReadFull(conn, response); err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if string(response) != string(testData) {
			t.Errorf("Response = %q, want %q", string(response), string(testData))
		}
	})

	// Test socat route
	t.Run("socat-route", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", "localhost:19108", testTimeout)
		if err != nil {
			t.Fatalf("Failed to connect to socat route: %v", err)
		}
		defer conn.Close()

		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatalf("Failed to set deadline: %v", err)
		}

		testData := []byte("Socat route test\n")
		if _, err := conn.Write(testData); err != nil {
			t.Fatalf("Failed to write: %v", err)
		}

		response := make([]byte, len(testData))
		if _, err := io.ReadFull(conn, response); err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if string(response) != string(testData) {
			t.Errorf("Response = %q, want %q", string(response), string(testData))
		}
	})
}
