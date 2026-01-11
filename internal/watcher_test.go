package internal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfigWatcher_DetectsFileChanges verifies the watcher detects config file changes
func TestConfigWatcher_DetectsFileChanges(t *testing.T) {
	// Create temp directory (isolated, auto-cleaned)
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	// Write initial config
	initialConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      test.localhost:
        context: test
        namespace: default
        service: test
        port: 80
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	// Load initial config
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Create a mock manager that tracks UpdateConfig calls
	manager := &Manager{
		config:  config,
		tunnels: make(map[string]*Tunnel),
	}

	// Create watcher
	watcher, err := NewConfigWatcher(configPath, config, manager, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	// Give watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Modify config file (direct write)
	updatedConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      updated.localhost:
        context: test
        namespace: default
        service: updated
        port: 8080
`
	if err := os.WriteFile(configPath, []byte(updatedConfig), 0644); err != nil {
		t.Fatalf("Failed to write updated config: %v", err)
	}

	// Wait for debounce (500ms) + processing
	time.Sleep(800 * time.Millisecond)

	// Verify manager has updated routes
	manager.mu.RLock()
	_, hasUpdatedRoute := manager.config.HTTP.K8s.Routes["updated.localhost"]
	_, hasOldRoute := manager.config.HTTP.K8s.Routes["test.localhost"]
	manager.mu.RUnlock()

	if !hasUpdatedRoute {
		t.Error("Expected manager config to have 'updated.localhost' route after reload")
	}

	if hasOldRoute {
		t.Error("Expected old 'test.localhost' route to be removed after reload")
	}
}

// TestConfigWatcher_DetectsAtomicRename verifies watcher detects atomic save (rename) operations
func TestConfigWatcher_DetectsAtomicRename(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	// Write initial config
	initialConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      initial.localhost:
        context: test
        namespace: default
        service: initial
        port: 80
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	manager := &Manager{
		config:  config,
		tunnels: make(map[string]*Tunnel),
	}

	watcher, err := NewConfigWatcher(configPath, config, manager, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	time.Sleep(100 * time.Millisecond)

	// Simulate atomic save (like vim/nano do): write to temp file, then rename
	tempFile := filepath.Join(tempDir, "test-config.yaml.tmp")
	atomicConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      atomic.localhost:
        context: test
        namespace: default
        service: atomic
        port: 9090
`
	if err := os.WriteFile(tempFile, []byte(atomicConfig), 0644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	// Atomic rename (this is what editors do)
	if err := os.Rename(tempFile, configPath); err != nil {
		t.Fatalf("Failed to rename config: %v", err)
	}

	// Wait for debounce + re-watch delay (100ms) + processing
	time.Sleep(1000 * time.Millisecond)

	// Verify updated route
	manager.mu.RLock()
	_, hasAtomicRoute := manager.config.HTTP.K8s.Routes["atomic.localhost"]
	_, hasInitialRoute := manager.config.HTTP.K8s.Routes["initial.localhost"]
	manager.mu.RUnlock()

	if !hasAtomicRoute {
		t.Error("Expected manager config to have 'atomic.localhost' route after atomic rename")
	}

	if hasInitialRoute {
		t.Error("Expected 'initial.localhost' route to be replaced after atomic rename")
	}
}

// TestConfigWatcher_InvalidConfigKeepsCurrent verifies invalid configs don't break things
func TestConfigWatcher_InvalidConfigKeepsCurrent(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	// Write valid initial config
	initialConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      valid.localhost:
        context: test
        namespace: default
        service: valid
        port: 80
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	manager := &Manager{
		config:  config,
		tunnels: make(map[string]*Tunnel),
	}

	watcher, err := NewConfigWatcher(configPath, config, manager, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	time.Sleep(100 * time.Millisecond)

	// Write invalid config (missing required fields)
	invalidConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      broken.localhost:
        # Missing context, namespace, service
        port: 80
`
	if err := os.WriteFile(configPath, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("Failed to write invalid config: %v", err)
	}

	// Wait for processing
	time.Sleep(800 * time.Millisecond)

	// Original config should be preserved
	manager.mu.RLock()
	_, hasValidRoute := manager.config.HTTP.K8s.Routes["valid.localhost"]
	_, hasBrokenRoute := manager.config.HTTP.K8s.Routes["broken.localhost"]
	manager.mu.RUnlock()

	if !hasValidRoute {
		t.Error("Expected original 'valid.localhost' route to be preserved after invalid config")
	}

	if hasBrokenRoute {
		t.Error("Expected 'broken.localhost' route NOT to be present (invalid config should be rejected)")
	}
}

// TestConfigWatcher_StopGracefully verifies watcher stops cleanly
func TestConfigWatcher_StopGracefully(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	initialConfig := `apiVersion: lazyfwd/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      test.localhost:
        context: test
        namespace: default
        service: test
        port: 80
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("Failed to write initial config: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	manager := &Manager{
		config:  config,
		tunnels: make(map[string]*Tunnel),
	}

	watcher, err := NewConfigWatcher(configPath, config, manager, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	time.Sleep(100 * time.Millisecond)

	// Stop should complete without hanging
	done := make(chan struct{})
	go func() {
		watcher.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success - stopped cleanly
	case <-time.After(2 * time.Second):
		t.Error("Watcher.Stop() timed out - possible deadlock")
	}
}
