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
			apiVersion: "autotunnel/v99",
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

	configContent := `apiVersion: autotunnel/v1

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

	if cfg.ApiVersion != "autotunnel/v1" {
		t.Errorf("expected apiVersion 'autotunnel/v1', got %q", cfg.ApiVersion)
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

	configContent := `apiVersion: autotunnel/v1

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

	configContent := `apiVersion: autotunnel/v1

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

	// Should have expanded ~ to home directory in ResolvedKubeconfigs
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".kube", "config")
	if len(cfg.HTTP.K8s.ResolvedKubeconfigs) != 1 {
		t.Fatalf("expected 1 resolved kubeconfig, got %d", len(cfg.HTTP.K8s.ResolvedKubeconfigs))
	}
	if cfg.HTTP.K8s.ResolvedKubeconfigs[0] != expected {
		t.Errorf("expected resolved kubeconfig %q, got %q", expected, cfg.HTTP.K8s.ResolvedKubeconfigs[0])
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
	configContent := `apiVersion: autotunnel/v1
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

func TestFileExists(t *testing.T) {
	tmpDir := t.TempDir()

	existingPath := filepath.Join(tmpDir, "exists.yaml")
	if err := os.WriteFile(existingPath, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if !FileExists(existingPath) {
		t.Error("expected FileExists to return true for existing file")
	}

	nonExistingPath := filepath.Join(tmpDir, "does-not-exist.yaml")
	if FileExists(nonExistingPath) {
		t.Error("expected FileExists to return false for non-existing file")
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

	// Verify kubeconfig defaults to empty (so $KUBECONFIG can be tried)
	if cfg.HTTP.K8s.Kubeconfig != "" {
		t.Errorf("expected empty kubeconfig default, got %q", cfg.HTTP.K8s.Kubeconfig)
	}

	// Verify Routes map is initialized (not nil)
	if cfg.HTTP.K8s.Routes == nil {
		t.Error("expected Routes map to be initialized, got nil")
	}
}

func TestResolveKubeconfigs_MultiplePaths(t *testing.T) {
	// Create a temporary config file with multiple colon-separated kubeconfig paths
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: autotunnel/v1

http:
  listen: ":9999"
  idle_timeout: 30m

  k8s:
    kubeconfig: ~/.kube/config:~/.kube/prod-config:/absolute/path/config

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

	// Should have 3 resolved paths
	if len(cfg.HTTP.K8s.ResolvedKubeconfigs) != 3 {
		t.Fatalf("expected 3 resolved kubeconfigs, got %d: %v", len(cfg.HTTP.K8s.ResolvedKubeconfigs), cfg.HTTP.K8s.ResolvedKubeconfigs)
	}

	home, _ := os.UserHomeDir()

	// First path should have ~ expanded
	expected1 := filepath.Join(home, ".kube", "config")
	if cfg.HTTP.K8s.ResolvedKubeconfigs[0] != expected1 {
		t.Errorf("expected first path %q, got %q", expected1, cfg.HTTP.K8s.ResolvedKubeconfigs[0])
	}

	// Second path should have ~ expanded
	expected2 := filepath.Join(home, ".kube", "prod-config")
	if cfg.HTTP.K8s.ResolvedKubeconfigs[1] != expected2 {
		t.Errorf("expected second path %q, got %q", expected2, cfg.HTTP.K8s.ResolvedKubeconfigs[1])
	}

	// Third path is absolute, should be unchanged
	expected3 := "/absolute/path/config"
	if cfg.HTTP.K8s.ResolvedKubeconfigs[2] != expected3 {
		t.Errorf("expected third path %q, got %q", expected3, cfg.HTTP.K8s.ResolvedKubeconfigs[2])
	}
}

func TestResolveKubeconfigs_EnvVarFallback(t *testing.T) {
	// Save and restore KUBECONFIG env var
	originalKubeconfig := os.Getenv("KUBECONFIG")
	defer func() {
		if originalKubeconfig != "" {
			os.Setenv("KUBECONFIG", originalKubeconfig)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	}()

	// Set KUBECONFIG env var
	os.Setenv("KUBECONFIG", "/env/path/config1:/env/path/config2")

	// Create a config WITHOUT kubeconfig specified
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: autotunnel/v1

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

	// Should fall back to $KUBECONFIG
	if len(cfg.HTTP.K8s.ResolvedKubeconfigs) != 2 {
		t.Fatalf("expected 2 resolved kubeconfigs from env var, got %d: %v", len(cfg.HTTP.K8s.ResolvedKubeconfigs), cfg.HTTP.K8s.ResolvedKubeconfigs)
	}

	if cfg.HTTP.K8s.ResolvedKubeconfigs[0] != "/env/path/config1" {
		t.Errorf("expected first path from env var, got %q", cfg.HTTP.K8s.ResolvedKubeconfigs[0])
	}
	if cfg.HTTP.K8s.ResolvedKubeconfigs[1] != "/env/path/config2" {
		t.Errorf("expected second path from env var, got %q", cfg.HTTP.K8s.ResolvedKubeconfigs[1])
	}
}

func TestResolveKubeconfigs_DefaultFallback(t *testing.T) {
	// Save and restore KUBECONFIG env var
	originalKubeconfig := os.Getenv("KUBECONFIG")
	defer func() {
		if originalKubeconfig != "" {
			os.Setenv("KUBECONFIG", originalKubeconfig)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	}()

	// Unset KUBECONFIG
	os.Unsetenv("KUBECONFIG")

	// Create a config WITHOUT kubeconfig specified
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: autotunnel/v1

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

	// Should fall back to default ~/.kube/config
	if len(cfg.HTTP.K8s.ResolvedKubeconfigs) != 1 {
		t.Fatalf("expected 1 resolved kubeconfig (default), got %d: %v", len(cfg.HTTP.K8s.ResolvedKubeconfigs), cfg.HTTP.K8s.ResolvedKubeconfigs)
	}

	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".kube", "config")
	if cfg.HTTP.K8s.ResolvedKubeconfigs[0] != expected {
		t.Errorf("expected default kubeconfig %q, got %q", expected, cfg.HTTP.K8s.ResolvedKubeconfigs[0])
	}
}

func TestResolveKubeconfigs_ExplicitOverridesEnv(t *testing.T) {
	// Save and restore KUBECONFIG env var
	originalKubeconfig := os.Getenv("KUBECONFIG")
	defer func() {
		if originalKubeconfig != "" {
			os.Setenv("KUBECONFIG", originalKubeconfig)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	}()

	// Set KUBECONFIG env var
	os.Setenv("KUBECONFIG", "/env/path/should-be-ignored")

	// Create a config WITH explicit kubeconfig
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: autotunnel/v1

http:
  listen: ":9999"
  idle_timeout: 30m

  k8s:
    kubeconfig: /explicit/path/config

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

	// Should use explicit path, not env var
	if len(cfg.HTTP.K8s.ResolvedKubeconfigs) != 1 {
		t.Fatalf("expected 1 resolved kubeconfig (explicit), got %d: %v", len(cfg.HTTP.K8s.ResolvedKubeconfigs), cfg.HTTP.K8s.ResolvedKubeconfigs)
	}

	if cfg.HTTP.K8s.ResolvedKubeconfigs[0] != "/explicit/path/config" {
		t.Errorf("expected explicit kubeconfig path, got %q", cfg.HTTP.K8s.ResolvedKubeconfigs[0])
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
	if !FileExists(configPath) {
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
	if !strings.Contains(string(content), "autotunnel/v1") {
		t.Error("expected config to contain 'autotunnel/v1'")
	}
}


// TestValidate_SocatRoute tests validation of socat routes
func TestValidate_SocatRoute(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			IdleTimeout: 60 * time.Minute,
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Service: "backend-api",
						},
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("unexpected error for valid socat route: %v", err)
	}
}

// TestValidate_SocatRouteMissingVia tests that socat routes require via config
func TestValidate_SocatRouteMissingVia(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via:       ViaConfig{}, // Neither pod nor service
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for socat route without via config")
	}
	if !strings.Contains(err.Error(), "via.pod or via.service is required") {
		t.Errorf("expected error about missing via, got: %v", err)
	}
}

// TestValidate_SocatRouteBothPodAndService tests that pod and service are mutually exclusive
func TestValidate_SocatRouteBothPodAndService(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Pod:     "bastion-pod",
							Service: "backend-api", // Both specified
						},
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for socat route with both pod and service")
	}
	if !strings.Contains(err.Error(), "cannot specify both via.pod and via.service") {
		t.Errorf("expected error about mutual exclusivity, got: %v", err)
	}
}

// TestValidate_SocatRouteMissingTarget tests that target host is required
func TestValidate_SocatRouteMissingTarget(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Service: "backend-api",
						},
						Target: TargetConfig{
							Host: "", // Missing
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for socat route without target host")
	}
	if !strings.Contains(err.Error(), "target.host is required") {
		t.Errorf("expected error about missing target host, got: %v", err)
	}
}

// TestValidate_SocatRouteInvalidTargetPort tests that target port must be valid
func TestValidate_SocatRouteInvalidTargetPort(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Service: "backend-api",
						},
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 0, // Invalid
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for socat route with invalid target port")
	}
	if !strings.Contains(err.Error(), "target.port must be between") {
		t.Errorf("expected error about invalid target port, got: %v", err)
	}
}

// TestValidate_PortCollisionBetweenRoutesAndSocat tests port collision detection
func TestValidate_PortCollisionBetweenRoutesAndSocat(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Routes: map[int]TCPRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Service:   "mysql",
						Port:      3306,
					},
				},
				Socat: map[int]SocatRouteConfig{
					3306: { // Same port - collision!
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Service: "backend-api",
						},
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for port collision between routes and socat")
	}
	if !strings.Contains(err.Error(), "port already used in tcp.k8s.routes") {
		t.Errorf("expected error about port collision, got: %v", err)
	}
}

// TestValidate_SocatRouteWithDirectPod tests socat route with direct pod targeting
func TestValidate_SocatRouteWithDirectPod(t *testing.T) {
	cfg := &Config{
		ApiVersion: CurrentApiVersion,
		HTTP: HTTPConfig{
			ListenAddr:  ":8989",
			IdleTimeout: 60 * time.Minute,
		},
		TCP: TCPConfig{
			K8s: TCPK8sConfig{
				Socat: map[int]SocatRouteConfig{
					3306: {
						Context:   "test-context",
						Namespace: "default",
						Via: ViaConfig{
							Pod:       "bastion-pod",
							Container: "main", // Optional container
						},
						Target: TargetConfig{
							Host: "mydb.rds.amazonaws.com",
							Port: 3306,
						},
					},
				},
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Errorf("unexpected error for socat route with direct pod: %v", err)
	}
}

// TestLoadConfig_WithSocatRoutes tests loading config with socat routes
func TestLoadConfig_WithSocatRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `apiVersion: autotunnel/v1

http:
  listen: ":8989"
  idle_timeout: 30m

tcp:
  idle_timeout: 60m
  k8s:
    socat:
      3306:
        context: eks-prod
        namespace: default
        via:
          service: backend-api
        target:
          host: mydb.cluster-xyz.us-east-1.rds.amazonaws.com
          port: 3306
      5432:
        context: eks-prod
        namespace: default
        via:
          pod: bastion-pod
          container: main
        target:
          host: 10.123.45.67
          port: 5432
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	// Verify socat routes were loaded
	if len(cfg.TCP.K8s.Socat) != 2 {
		t.Fatalf("expected 2 socat routes, got %d", len(cfg.TCP.K8s.Socat))
	}

	// Verify MySQL route
	mysql, ok := cfg.TCP.K8s.Socat[3306]
	if !ok {
		t.Fatal("expected socat route for port 3306")
	}
	if mysql.Context != "eks-prod" {
		t.Errorf("expected context 'eks-prod', got %q", mysql.Context)
	}
	if mysql.Via.Service != "backend-api" {
		t.Errorf("expected via.service 'backend-api', got %q", mysql.Via.Service)
	}
	if mysql.Target.Host != "mydb.cluster-xyz.us-east-1.rds.amazonaws.com" {
		t.Errorf("expected target.host 'mydb.cluster-xyz.us-east-1.rds.amazonaws.com', got %q", mysql.Target.Host)
	}

	// Verify PostgreSQL route
	postgres, ok := cfg.TCP.K8s.Socat[5432]
	if !ok {
		t.Fatal("expected socat route for port 5432")
	}
	if postgres.Via.Pod != "bastion-pod" {
		t.Errorf("expected via.pod 'bastion-pod', got %q", postgres.Via.Pod)
	}
	if postgres.Via.Container != "main" {
		t.Errorf("expected via.container 'main', got %q", postgres.Via.Container)
	}
}
