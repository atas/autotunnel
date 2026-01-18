package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
)

// TestConfigWatcher_DetectsFileChanges verifies the watcher detects config file changes
func TestConfigWatcher_DetectsFileChanges(t *testing.T) {
	// Create temp directory (isolated, auto-cleaned)
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	// Write initial config
	initialConfig := `apiVersion: autotunnel/v1
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
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Create watcher
	watcher, err := NewConfigWatcher(configPath, cfg, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	// Give watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Modify config file (direct write)
	updatedConfig := `apiVersion: autotunnel/v1
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

	// Wait for reload signal
	select {
	case <-watcher.ReloadChan:
		// Success - reload was triggered
	case <-time.After(2 * time.Second):
		t.Error("Expected reload signal not received")
	}

	// Verify watcher has updated config
	currentCfg := watcher.GetConfig()
	_, hasUpdatedRoute := currentCfg.HTTP.K8s.Routes["updated.localhost"]
	_, hasOldRoute := currentCfg.HTTP.K8s.Routes["test.localhost"]

	if !hasUpdatedRoute {
		t.Error("Expected watcher config to have 'updated.localhost' route after reload")
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
	initialConfig := `apiVersion: autotunnel/v1
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

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	watcher, err := NewConfigWatcher(configPath, cfg, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	time.Sleep(100 * time.Millisecond)

	// Simulate atomic save (like vim/nano do): write to temp file, then rename
	tempFile := filepath.Join(tempDir, "test-config.yaml.tmp")
	atomicConfig := `apiVersion: autotunnel/v1
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

	// Wait for reload signal
	select {
	case <-watcher.ReloadChan:
		// Success - reload was triggered
	case <-time.After(2 * time.Second):
		t.Error("Expected reload signal not received after atomic rename")
	}

	// Verify updated route
	currentCfg := watcher.GetConfig()
	_, hasAtomicRoute := currentCfg.HTTP.K8s.Routes["atomic.localhost"]
	_, hasInitialRoute := currentCfg.HTTP.K8s.Routes["initial.localhost"]

	if !hasAtomicRoute {
		t.Error("Expected watcher config to have 'atomic.localhost' route after atomic rename")
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
	initialConfig := `apiVersion: autotunnel/v1
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

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	watcher, err := NewConfigWatcher(configPath, cfg, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	time.Sleep(100 * time.Millisecond)

	// Write invalid config (missing required fields)
	invalidConfig := `apiVersion: autotunnel/v1
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

	// Wait for processing - should NOT receive reload signal (invalid config rejected)
	select {
	case <-watcher.ReloadChan:
		t.Error("Should NOT receive reload signal for invalid config")
	case <-time.After(1 * time.Second):
		// Expected - no reload signal for invalid config
	}

	// Original config should be preserved
	currentCfg := watcher.GetConfig()
	_, hasValidRoute := currentCfg.HTTP.K8s.Routes["valid.localhost"]
	_, hasBrokenRoute := currentCfg.HTTP.K8s.Routes["broken.localhost"]

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

	initialConfig := `apiVersion: autotunnel/v1
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

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	watcher, err := NewConfigWatcher(configPath, cfg, false)
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

// TestConfigWatcher_PreservesCliVerbose verifies that CLI --verbose flag is preserved across reloads
func TestConfigWatcher_PreservesCliVerbose(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "test-config.yaml")

	// Write config WITHOUT verbose flag (defaults to false)
	initialConfig := `apiVersion: autotunnel/v1
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

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Simulate CLI --verbose flag
	cfg.Verbose = true

	// Create watcher with cliVerbose=true (simulating --verbose flag was passed)
	watcher, err := NewConfigWatcher(configPath, cfg, true)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}

	watcher.Start()
	defer watcher.Stop()

	time.Sleep(100 * time.Millisecond)

	// Modify config (still without verbose flag)
	updatedConfig := `apiVersion: autotunnel/v1
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

	// Wait for reload signal
	select {
	case <-watcher.ReloadChan:
		// Success - reload was triggered
	case <-time.After(2 * time.Second):
		t.Error("Expected reload signal not received")
	}

	// Verify that Verbose is still true (preserved from CLI flag)
	currentCfg := watcher.GetConfig()
	if !currentCfg.Verbose {
		t.Error("Expected Verbose to be true after reload (CLI flag should be preserved)")
	}

	// Also verify the config was actually reloaded
	_, hasUpdatedRoute := currentCfg.HTTP.K8s.Routes["updated.localhost"]
	if !hasUpdatedRoute {
		t.Error("Expected config to be reloaded with new route")
	}
}
