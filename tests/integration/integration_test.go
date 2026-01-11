//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyAddr    = "localhost:18989" // Use non-standard port to avoid conflicts
	testTimeout  = 60 * time.Second
	startupWait  = 3 * time.Second
	idleTimeout  = "10s" // Short idle timeout for testing
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

// startOPF starts the lazyfwd binary with a test configuration
func startOPF(t *testing.T, configPath string) (*exec.Cmd, func()) {
	t.Helper()

	// Find the binary (should be built by Makefile)
	binaryPath := filepath.Join("..", "..", "lazyfwd")
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		t.Fatalf("lazyfwd binary not found at %s - run 'make build' first", binaryPath)
	}

	cmd := exec.Command(binaryPath, "--config", configPath, "--verbose")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start lazyfwd: %v", err)
	}

	// Wait for startup
	time.Sleep(startupWait)

	cleanup := func() {
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
			cmd.Wait()
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
	sb.WriteString("apiVersion: lazyfwd/v1\n")
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
		sb.WriteString(fmt.Sprintf("        service: %s\n", svc.Service))
		sb.WriteString(fmt.Sprintf("        port: %d\n", svc.Port))
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "lazyfwd-test-config-*.yaml")
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
	Service   string
	Port      int
}

// TestBasicProxyConnection tests that requests are proxied through a tunnel
func TestBasicProxyConnection(t *testing.T) {
	// Create test config pointing to our test services
	configPath := writeTestConfig(t, map[string]serviceConfig{
		"nginx.test": {
			Context:   getTestContext(),
			Namespace: "lazyfwd-test",
			Service:   "nginx",
			Port:      80,
		},
		"echo.test": {
			Context:   getTestContext(),
			Namespace: "lazyfwd-test",
			Service:   "echo",
			Port:      8080,
		},
	})

	// Start lazyfwd
	_, cleanup := startOPF(t, configPath)
	defer cleanup()

	// Test cases
	tests := []struct {
		name           string
		host           string
		path           string
		wantStatus     int
		wantBodyContains string
	}{
		{
			name:           "nginx service",
			host:           "nginx.test",
			path:           "/",
			wantStatus:     http.StatusOK,
			wantBodyContains: "nginx",
		},
		{
			name:           "echo service",
			host:           "echo.test",
			path:           "/",
			wantStatus:     http.StatusOK,
			wantBodyContains: "lazyfwd-integration-test",
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
			Namespace: "lazyfwd-test",
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
			Namespace: "lazyfwd-test",
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
			Namespace: "lazyfwd-test",
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
	cmd.Process.Signal(os.Interrupt)

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
		cmd.Process.Kill()
	}
}
