package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate_ApiVersion(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		wantErr    bool
		errContain string
	}{
		{
			name:       "empty apiVersion defaults to current",
			apiVersion: "",
			wantErr:    false,
		},
		{
			name:       "current apiVersion is valid",
			apiVersion: CurrentApiVersion,
			wantErr:    false,
		},
		{
			name:       "unsupported apiVersion fails",
			apiVersion: "lazyfwd/v99",
			wantErr:    true,
			errContain: "unsupported config apiVersion",
		},
		{
			name:       "wrong format apiVersion fails",
			apiVersion: "v1",
			wantErr:    true,
			errContain: "unsupported config apiVersion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ApiVersion: tt.apiVersion,
				HTTP: HTTPConfig{
					ListenAddr:  ":8989",
					IdleTimeout: 60 * time.Minute,
					K8s: K8sConfig{
						Routes: map[string]K8sRouteConfig{
							"test.localhost": {
								Context:   "test-context",
								Namespace: "default",
								Service:   "test-service",
								Port:      8080,
							},
						},
					},
				},
			}

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
					return
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("expected error to contain %q, got %q", tt.errContain, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestLoadConfig_WithNewFormat(t *testing.T) {
	// Create a temporary config file with new format
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: lazyfwd/v1

http:
  listen: ":9999"
  idle_timeout: 30m

  k8s:
    routes:
      test.localhost:
        context: test
        namespace: default
        service: test-svc
        port: 80
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.ApiVersion != "lazyfwd/v1" {
		t.Errorf("expected apiVersion 'lazyfwd/v1', got %q", cfg.ApiVersion)
	}

	if cfg.HTTP.ListenAddr != ":9999" {
		t.Errorf("expected listen ':9999', got %q", cfg.HTTP.ListenAddr)
	}

	if cfg.HTTP.IdleTimeout != 30*time.Minute {
		t.Errorf("expected idle_timeout 30m, got %v", cfg.HTTP.IdleTimeout)
	}

	if len(cfg.HTTP.K8s.Routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(cfg.HTTP.K8s.Routes))
	}

	route, ok := cfg.HTTP.K8s.Routes["test.localhost"]
	if !ok {
		t.Fatal("expected route 'test.localhost' not found")
	}

	if route.Context != "test" {
		t.Errorf("expected context 'test', got %q", route.Context)
	}

	// Note: Validate() is now called inside LoadConfig(), so we don't need to call it again
	// but calling it again is harmless
	if err := cfg.Validate(); err != nil {
		t.Errorf("validation failed: %v", err)
	}
}

func TestLoadConfig_WithKubeconfig(t *testing.T) {
	// Create a temporary config file with kubeconfig
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: lazyfwd/v1

http:
  listen: ":9999"
  idle_timeout: 30m

  k8s:
    kubeconfig: /custom/path/kubeconfig

    routes:
      test.localhost:
        context: test
        namespace: default
        service: test-svc
        port: 80
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.HTTP.K8s.Kubeconfig != "/custom/path/kubeconfig" {
		t.Errorf("expected kubeconfig '/custom/path/kubeconfig', got %q", cfg.HTTP.K8s.Kubeconfig)
	}
}

func TestLoadConfig_KubeconfigTildeExpansion(t *testing.T) {
	// Create a temporary config file with ~ in kubeconfig path
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: lazyfwd/v1

http:
  listen: ":9999"
  idle_timeout: 30m

  k8s:
    kubeconfig: ~/.kube/config

    routes:
      test.localhost:
        context: test
        namespace: default
        service: test-svc
        port: 80
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	// Should have expanded ~ to home directory
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".kube", "config")
	if cfg.HTTP.K8s.Kubeconfig != expected {
		t.Errorf("expected kubeconfig %q, got %q", expected, cfg.HTTP.K8s.Kubeconfig)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "default",
						Service:   "test-svc",
						Port:      0, // Invalid
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for invalid port")
	}
	if !strings.Contains(err.Error(), "port must be between") {
		t.Errorf("expected error about port, got: %v", err)
	}
}

func TestValidate_PodOnlyRoute(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"debug.localhost": {
						Context:   "test",
						Namespace: "default",
						Pod:       "my-debug-pod", // Pod instead of Service
						Port:      8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("unexpected error for pod-only route: %v", err)
	}
}

func TestValidate_NoServiceOrPod(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "default",
						// Neither Service nor Pod specified
						Port: 8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error when neither service nor pod is specified")
	}
	if !strings.Contains(err.Error(), "either service or pod is required") {
		t.Errorf("expected error about missing service/pod, got: %v", err)
	}
}

func TestValidate_BothServiceAndPod(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "default",
						Service:   "my-service",
						Pod:       "my-pod", // Both specified - should fail
						Port:      8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error when both service and pod are specified")
	}
	if !strings.Contains(err.Error(), "cannot specify both service and pod") {
		t.Errorf("expected error about mutual exclusivity, got: %v", err)
	}
}

func TestValidate_EmptyListenAddr(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  "", // Empty - should fail
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "default",
						Service:   "test-svc",
						Port:      8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for empty listen address")
	}
	if !strings.Contains(err.Error(), "http.listen is required") {
		t.Errorf("expected error about listen address, got: %v", err)
	}
}

func TestValidate_InvalidIdleTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{"zero timeout", 0},
		{"negative timeout", -1 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ApiVersion: CurrentApiVersion,
				HTTP: HTTPConfig{
					ListenAddr:  ":8989",
					IdleTimeout: tt.timeout,
					K8s: K8sConfig{
						Routes: map[string]K8sRouteConfig{
							"test.localhost": {
								Context:   "test",
								Namespace: "default",
								Service:   "test-svc",
								Port:      8080,
							},
						},
					},
				},
			}

			err := cfg.Validate()
			if err == nil {
				t.Error("expected error for invalid idle timeout")
			}
			if !strings.Contains(err.Error(), "idle_timeout must be positive") {
				t.Errorf("expected error about idle timeout, got: %v", err)
			}
		})
	}
}

func TestValidate_EmptyContext(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "", // Empty - should fail
						Namespace: "default",
						Service:   "test-svc",
						Port:      8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for empty context")
	}
	if !strings.Contains(err.Error(), "context is required") {
		t.Errorf("expected error about context, got: %v", err)
	}
}

func TestValidate_EmptyNamespace(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "", // Empty - should fail
						Service:   "test-svc",
						Port:      8080,
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for empty namespace")
	}
	if !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("expected error about namespace, got: %v", err)
	}
}

func TestValidate_PortTooHigh(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
			K8s: K8sConfig{
				Routes: map[string]K8sRouteConfig{
					"test.localhost": {
						Context:   "test",
						Namespace: "default",
						Service:   "test-svc",
						Port:      65536, // Too high - should fail
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for port too high")
	}
	if !strings.Contains(err.Error(), "port must be between") {
		t.Errorf("expected error about port, got: %v", err)
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
	if !strings.Contains(err.Error(), "failed to read config file") {
		t.Errorf("expected error about reading file, got: %v", err)
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write invalid YAML
	if err := os.WriteFile(configPath, []byte("{{{invalid yaml"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "failed to parse config file") {
		t.Errorf("expected error about parsing, got: %v", err)
	}
}

func TestLoadConfig_ValidationFailure(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write valid YAML but invalid config (missing listen)
	configContent := `apiVersion: lazyfwd/v1
http:
  idle_timeout: 30m
  k8s:
    routes:
      test.localhost:
        context: test
        namespace: default
        service: test-svc
        port: 80
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected validation error")
	}
	if !strings.Contains(err.Error(), "http.listen is required") {
		t.Errorf("expected validation error, got: %v", err)
	}
}

func TestConfigExists(t *testing.T) {
	tmpDir := t.TempDir()

	// Test with existing file
	existingPath := filepath.Join(tmpDir, "exists.yaml")
	if err := os.WriteFile(existingPath, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if !ConfigExists(existingPath) {
		t.Error("expected ConfigExists to return true for existing file")
	}

	// Test with non-existing file
	nonExistingPath := filepath.Join(tmpDir, "does-not-exist.yaml")
	if ConfigExists(nonExistingPath) {
		t.Error("expected ConfigExists to return false for non-existing file")
	}
}

func TestShouldAutoReload(t *testing.T) {
	tests := []struct {
		name     string
		value    *bool
		expected bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{AutoReloadConfig: tt.value}
			if got := cfg.ShouldAutoReload(); got != tt.expected {
				t.Errorf("ShouldAutoReload() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Verify kubeconfig defaults to ~/.kube/config
	home, _ := os.UserHomeDir()
	expectedKubeconfig := filepath.Join(home, ".kube", "config")
	if cfg.HTTP.K8s.Kubeconfig != expectedKubeconfig {
		t.Errorf("expected kubeconfig %q, got %q", expectedKubeconfig, cfg.HTTP.K8s.Kubeconfig)
	}

	// Verify Routes map is initialized (not nil)
	if cfg.HTTP.K8s.Routes == nil {
		t.Error("expected Routes map to be initialized, got nil")
	}
}

func TestCreateDefaultConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "new-config.yaml")

	err := CreateDefaultConfig(configPath)
	if err != nil {
		t.Fatalf("CreateDefaultConfig failed: %v", err)
	}

	// Verify file exists
	if !ConfigExists(configPath) {
		t.Error("expected config file to be created")
	}

	// Verify file has content
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read created config: %v", err)
	}
	if len(content) == 0 {
		t.Error("expected config file to have content")
	}

	// Verify it contains expected markers
	if !strings.Contains(string(content), "apiVersion") {
		t.Error("expected config to contain 'apiVersion'")
	}
	if !strings.Contains(string(content), "lazyfwd/v1") {
		t.Error("expected config to contain 'lazyfwd/v1'")
	}
}
