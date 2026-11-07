package watcher

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// routeConfig returns a valid config with a single HTTP route for host.
func routeConfig(host string) string {
	return `apiVersion: autotunnel/v1
http:
  listen: ":8989"
  idle_timeout: 60m
  k8s:
    routes:
      ` + host + `:
        context: test
        namespace: default
        service: test
        port: 80
`
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write %s: %v", path, err)
	}
}

// atomicSave writes through a temp file and renames over path, like sed -i and dotfile managers.
func atomicSave(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	writeFile(t, tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("Failed to rename over %s: %v", path, err)
	}
}

// retarget atomically points the symlink at link to target.
func retarget(t *testing.T, link, target string) {
	t.Helper()
	tmp := link + ".tmp"
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatalf("Failed to create symlink: %v", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatalf("Failed to rename symlink over %s: %v", link, err)
	}
}

func startWatcher(t *testing.T, configPath string, cliVerbose bool) *ConfigWatcher {
	t.Helper()
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if cliVerbose {
		cfg.Verbose = true // main.go applies the flag before creating the watcher
	}
	w, err := NewConfigWatcher(configPath, cfg, cliVerbose)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}
	w.Start()
	t.Cleanup(w.Stop)
	time.Sleep(100 * time.Millisecond)
	return w
}

// expectReload waits for a reload signal and checks the new config has the route for host.
func expectReload(t *testing.T, w *ConfigWatcher, host string) {
	t.Helper()
	select {
	case <-w.ReloadChan:
	case <-time.After(4 * time.Second):
		t.Fatalf("Expected reload signal for %s", host)
	}
	if _, ok := w.GetConfig().HTTP.K8s.Routes[host]; !ok {
		t.Fatalf("Expected config to have route %s, got %v", host, w.GetConfig().HTTP.K8s.Routes)
	}
}

func expectNoReload(t *testing.T, w *ConfigWatcher) {
	t.Helper()
	select {
	case <-w.ReloadChan:
		t.Fatal("Unexpected reload signal")
	case <-time.After(1200 * time.Millisecond): // debounce is 500ms
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog collects log output until the test ends.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

func TestConfigWatcher_ConsecutiveAtomicSaves(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)

	for _, host := range []string{"one.localhost", "two.localhost", "three.localhost"} {
		atomicSave(t, configPath, routeConfig(host))
		expectReload(t, w, host)
	}
}

func TestConfigWatcher_DeleteAndRecreate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, w) // the missing file is logged and the current config stays

	writeFile(t, configPath, routeConfig("recreated.localhost"))
	expectReload(t, w, "recreated.localhost")

	writeFile(t, configPath, routeConfig("edited.localhost"))
	expectReload(t, w, "edited.localhost")
}

func TestConfigWatcher_MoveAwayAndRecreate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)

	if err := os.Rename(configPath, configPath+".bak"); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, w)

	writeFile(t, configPath, routeConfig("recreated.localhost"))
	expectReload(t, w, "recreated.localhost")

	writeFile(t, configPath, routeConfig("edited.localhost"))
	expectReload(t, w, "edited.localhost")
}

func TestConfigWatcher_RelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "cfg.yaml", routeConfig("initial.localhost"))
	w := startWatcher(t, "cfg.yaml", false)

	for _, host := range []string{"one.localhost", "two.localhost"} {
		atomicSave(t, "cfg.yaml", routeConfig(host))
		expectReload(t, w, host)
	}
}

func TestConfigWatcher_IgnoresOtherFilesInDirectory(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	logs := captureLog(t)
	w := startWatcher(t, configPath, false)

	other := filepath.Join(dir, "other.yaml")
	writeFile(t, other, routeConfig("other.localhost"))
	writeFile(t, other, routeConfig("other2.localhost"))
	atomicSave(t, other, routeConfig("other3.localhost"))
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, w)

	// reloadConfig drops an unchanged config, so only the log shows it was not even loaded
	if strings.Contains(logs.String(), "Config file changed") {
		t.Errorf("Expected other files not to trigger a reload, got: %s", logs.String())
	}

	writeFile(t, configPath, routeConfig("changed.localhost"))
	expectReload(t, w, "changed.localhost")
	if !strings.Contains(logs.String(), "Config file changed") {
		t.Errorf("Expected the config's own change to be logged, got: %s", logs.String())
	}
}

func TestConfigWatcher_SymlinkTargetEdits(t *testing.T) {
	linkDir, targetDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(linkDir, "cfg.yaml")
	target := filepath.Join(targetDir, "real.yaml")
	writeFile(t, target, routeConfig("initial.localhost"))
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, configPath, false)

	writeFile(t, target, routeConfig("written.localhost"))
	expectReload(t, w, "written.localhost")

	for _, host := range []string{"atomic1.localhost", "atomic2.localhost"} {
		atomicSave(t, target, routeConfig(host))
		expectReload(t, w, host)
	}
}

func TestConfigWatcher_SymlinkRetarget(t *testing.T) {
	linkDir, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
	configPath := filepath.Join(linkDir, "cfg.yaml")
	oldTarget := filepath.Join(oldDir, "real.yaml")
	newTarget := filepath.Join(newDir, "real.yaml")
	writeFile(t, oldTarget, routeConfig("old.localhost"))
	writeFile(t, newTarget, routeConfig("new.localhost"))
	if err := os.Symlink(oldTarget, configPath); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, configPath, false)

	retarget(t, configPath, newTarget)
	expectReload(t, w, "new.localhost")

	// The new target is followed
	atomicSave(t, newTarget, routeConfig("new-edited.localhost"))
	expectReload(t, w, "new-edited.localhost")

	// The old target no longer matters
	writeFile(t, oldTarget, routeConfig("old-edited.localhost"))
	expectNoReload(t, w)

	// The link can move back
	retarget(t, configPath, oldTarget)
	expectReload(t, w, "old-edited.localhost")
}

// Kubernetes ConfigMap mounts: cfg.yaml -> ..data/cfg.yaml and ..data -> ..v1. Only ..data changes.
func TestConfigWatcher_IntermediateSymlinkRetarget(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cfg.yaml")
	for _, version := range []string{"..v1", "..v2"} {
		if err := os.Mkdir(filepath.Join(dir, version), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "..v1", "cfg.yaml"), routeConfig("v1.localhost"))
	writeFile(t, filepath.Join(dir, "..v2", "cfg.yaml"), routeConfig("v2.localhost"))
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "cfg.yaml"), configPath); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, configPath, false)

	retarget(t, filepath.Join(dir, "..data"), "..v2")
	expectReload(t, w, "v2.localhost")

	writeFile(t, filepath.Join(dir, "..v2", "cfg.yaml"), routeConfig("v2-edited.localhost"))
	expectReload(t, w, "v2-edited.localhost")
}

// cfg.yaml -> chain/link.yaml -> real.yaml. The link in chain moves, so chain must be watched too.
func TestConfigWatcher_RetargetInUnwatchedDirectory(t *testing.T) {
	linkDir, chainDir, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	configPath := filepath.Join(linkDir, "cfg.yaml")
	chainLink := filepath.Join(chainDir, "link.yaml")
	oldTarget := filepath.Join(oldDir, "real.yaml")
	newTarget := filepath.Join(newDir, "real.yaml")
	writeFile(t, oldTarget, routeConfig("old.localhost"))
	writeFile(t, newTarget, routeConfig("new.localhost"))
	if err := os.Symlink(oldTarget, chainLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(chainLink, configPath); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, configPath, false)

	retarget(t, chainLink, newTarget)
	expectReload(t, w, "new.localhost")

	// The new target is followed
	atomicSave(t, newTarget, routeConfig("new-edited.localhost"))
	expectReload(t, w, "new-edited.localhost")
}

// Watching the home directory must not open everything in it, as fsnotify's kqueue backend does on
// macOS, including the folders macOS protects.
func TestConfigWatcher_DoesNotOpenDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	for i := range 200 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("file%d", i)), "x")
		if err := os.Mkdir(filepath.Join(dir, fmt.Sprintf("dir%d", i)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))

	before := openFiles(t)
	w := startWatcher(t, configPath, false)
	if opened := openFiles(t) - before; opened > 10 {
		t.Errorf("Watcher opened %d files for a directory of 401 entries", opened)
	}

	atomicSave(t, configPath, routeConfig("changed.localhost"))
	expectReload(t, w, "changed.localhost")
}

func openFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("Cannot count open files: %v", err)
	}
	return len(entries)
}

// The watch on a symlink's target must move to the file that replaced it
func TestConfigWatcher_SymlinkTargetWriteAfterReplace(t *testing.T) {
	linkDir, targetDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(linkDir, "cfg.yaml")
	target := filepath.Join(targetDir, "real.yaml")
	writeFile(t, target, routeConfig("initial.localhost"))
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatal(err)
	}
	w := startWatcher(t, configPath, false)

	atomicSave(t, target, routeConfig("replaced.localhost"))
	expectReload(t, w, "replaced.localhost")
	writeFile(t, target, routeConfig("written.localhost"))
	expectReload(t, w, "written.localhost")
}

func holdPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port, l
}

func withTCPRoute(content string, port int) string {
	return content + fmt.Sprintf(`tcp:
  idle_timeout: 60m
  k8s:
    routes:
      %d:
        context: test
        namespace: default
        service: test
        port: 5432
`, port)
}

func withJumpRoute(content string, port int) string {
	return content + fmt.Sprintf(`tcp:
  idle_timeout: 60m
  k8s:
    jump:
      %d:
        context: test
        namespace: default
        via:
          service: test
        target:
          host: db.example.com
          port: 5432
`, port)
}

func withListen(content, addr string) string {
	return strings.Replace(content, `":8989"`, strconv.Quote(addr), 1)
}

func TestConfigWatcher_BusyPortKeepsCurrent(t *testing.T) {
	tests := []struct {
		name string
		edit func(port int) string
	}{
		{"tcp route", func(port int) string { return withTCPRoute(routeConfig("new.localhost"), port) }},
		{"jump route", func(port int) string { return withJumpRoute(routeConfig("new.localhost"), port) }},
		{"http listen", func(port int) string {
			return withListen(routeConfig("new.localhost"), fmt.Sprintf("127.0.0.1:%d", port))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLog(t)
			configPath := filepath.Join(t.TempDir(), "cfg.yaml")
			writeFile(t, configPath, routeConfig("initial.localhost"))
			w := startWatcher(t, configPath, false)
			current := w.GetConfig()

			port, holder := holdPort(t)
			edited := tt.edit(port)
			writeFile(t, configPath, edited)
			expectNoReload(t, w)

			if w.GetConfig() != current {
				t.Error("Expected current config to be kept")
			}
			if !strings.Contains(logs.String(), strconv.Itoa(port)) || !strings.Contains(logs.String(), "keeping current config") {
				t.Errorf("Expected the busy port to be logged, got: %s", logs.String())
			}

			// Saving the same file once the port is free applies it
			_ = holder.Close()
			writeFile(t, configPath, edited)
			expectReload(t, w, "new.localhost")
		})
	}
}

func TestConfigWatcher_UnusableListenAddressKeepsCurrent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)
	current := w.GetConfig()

	writeFile(t, configPath, withListen(routeConfig("new.localhost"), "127.0.0.1:99999"))
	expectNoReload(t, w)

	if w.GetConfig() != current {
		t.Error("Expected current config to be kept")
	}
}

// Ports the running config holds are busy because of ourselves, so they must not block a reload.
func TestConfigWatcher_PortsWeHoldDoNotBlockReload(t *testing.T) {
	tcpPort, _ := holdPort(t)
	httpPort, _ := holdPort(t)
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, configPath, withTCPRoute(withListen(routeConfig("initial.localhost"), httpAddr), tcpPort))
	w := startWatcher(t, configPath, false)

	// Same ports, different route
	writeFile(t, configPath, withTCPRoute(withListen(routeConfig("same-ports.localhost"), httpAddr), tcpPort))
	expectReload(t, w, "same-ports.localhost")

	// HTTP moves to another interface on the port it already holds
	writeFile(t, configPath, withTCPRoute(withListen(routeConfig("same-port.localhost"), fmt.Sprintf("localhost:%d", httpPort)), tcpPort))
	expectReload(t, w, "same-port.localhost")

	// The HTTP and TCP ports swap
	writeFile(t, configPath, withTCPRoute(withListen(routeConfig("swapped.localhost"), fmt.Sprintf("127.0.0.1:%d", tcpPort)), httpPort))
	expectReload(t, w, "swapped.localhost")
}

func TestConfigWatcher_SkipsUnchangedConfig(t *testing.T) {
	content := routeConfig("initial.localhost")
	tests := []struct {
		name string
		save func(t *testing.T, path string)
	}{
		{"chmod", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"touch", func(t *testing.T, path string) {
			now := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, now, now); err != nil {
				t.Fatal(err)
			}
		}},
		{"comment", func(t *testing.T, path string) { writeFile(t, path, "# a comment\n"+content) }},
		{"identical save", func(t *testing.T, path string) { writeFile(t, path, content) }},
		{"identical atomic save", func(t *testing.T, path string) { atomicSave(t, path, content) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "cfg.yaml")
			writeFile(t, configPath, content)
			w := startWatcher(t, configPath, false)
			current := w.GetConfig()

			tt.save(t, configPath)
			expectNoReload(t, w)
			if w.GetConfig() != current {
				t.Error("Expected current config to be kept")
			}

			// A real change right after still goes through
			writeFile(t, configPath, routeConfig("changed.localhost"))
			expectReload(t, w, "changed.localhost")
		})
	}
}

func TestConfigWatcher_VerboseComparison(t *testing.T) {
	plain := routeConfig("initial.localhost")
	verbose := "verbose: true\n" + plain

	t.Run("cli flag, same file", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "cfg.yaml")
		writeFile(t, configPath, plain)
		w := startWatcher(t, configPath, true)

		writeFile(t, configPath, plain)
		expectNoReload(t, w)
	})

	t.Run("cli flag, file turns verbose on", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "cfg.yaml")
		writeFile(t, configPath, plain)
		w := startWatcher(t, configPath, true)

		writeFile(t, configPath, verbose)
		expectNoReload(t, w) // already verbose because of the flag
	})

	t.Run("no cli flag, file turns verbose on", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "cfg.yaml")
		writeFile(t, configPath, plain)
		w := startWatcher(t, configPath, false)

		writeFile(t, configPath, verbose)
		expectReload(t, w, "initial.localhost")
		if !w.GetConfig().Verbose {
			t.Error("Expected verbose to be on")
		}
	})
}

// A file in the same directory written more often than the debounce, like a shell history in $HOME,
// must not keep pushing the reload back.
func TestConfigWatcher_BusyNeighbourDoesNotDelayReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		busy := filepath.Join(dir, "busy.log")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				// Saved through a rename, as shells and editors do, so the directory itself changes
				_ = os.WriteFile(busy+".tmp", []byte(strconv.Itoa(i)), 0644)
				_ = os.Rename(busy+".tmp", busy)
			}
		}
	}()
	defer func() { close(stop); <-done }()

	time.Sleep(200 * time.Millisecond)
	writeFile(t, configPath, routeConfig("changed.localhost"))
	expectReload(t, w, "changed.localhost")
}

func TestConfigWatcher_RestoreConfigKeepsNewerConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)
	previous := w.GetConfig()

	writeFile(t, configPath, routeConfig("failed.localhost"))
	expectReload(t, w, "failed.localhost")
	failed := w.GetConfig()

	// A newer config arrives while the failed one is being shut down
	writeFile(t, configPath, routeConfig("newer.localhost"))
	expectReload(t, w, "newer.localhost")

	if w.RestoreConfig(failed, previous) {
		t.Fatal("Expected RestoreConfig to keep the newer config")
	}
	if _, ok := w.GetConfig().HTTP.K8s.Routes["newer.localhost"]; !ok {
		t.Fatalf("Expected the newer config to stay, got %v", w.GetConfig().HTTP.K8s.Routes)
	}

	if !w.RestoreConfig(w.GetConfig(), previous) || w.GetConfig() != previous {
		t.Fatal("Expected RestoreConfig to put the previous config back")
	}
}

// A directory on the way to the config can be deleted and created again, as a dotfiles tool or
// restoring a backup does. Its parent is watched while it is gone, so the new one is watched too.
func TestConfigWatcher_DirectoryRecreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dotfiles")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "cfg.yaml")
	writeFile(t, configPath, routeConfig("initial.localhost"))
	w := startWatcher(t, configPath, false)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	expectNoReload(t, w) // the missing file is logged and the current config stays

	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the watcher pick up the new directory
	writeFile(t, configPath, routeConfig("recreated.localhost"))
	expectReload(t, w, "recreated.localhost")

	writeFile(t, configPath, routeConfig("edited.localhost"))
	expectReload(t, w, "edited.localhost")
}

// A directory on the way to the config that can't be watched (execute-only here, or kept by
// macOS privacy protection) is logged once and skipped. The rest is still watched.
func TestConfigWatcher_UnwatchableDirectoryKeepsTheRest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open any directory")
	}
	root := t.TempDir()
	linkDir := filepath.Join(root, "links")
	locked := filepath.Join(root, "locked")
	open := filepath.Join(root, "open")
	for _, dir := range []string{linkDir, locked, open} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(locked, "cfg.yaml"), routeConfig("locked.localhost"))
	writeFile(t, filepath.Join(open, "cfg.yaml"), routeConfig("open.localhost"))
	if err := os.Chmod(locked, 0111); err != nil { // can be passed through, not listed
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0755) })

	configPath := filepath.Join(linkDir, "cfg.yaml")
	if err := os.Symlink(filepath.Join(locked, "cfg.yaml"), configPath); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	w := startWatcher(t, configPath, false)

	// Events in the watched directory set up the watches again, which fails again
	writeFile(t, filepath.Join(linkDir, "other1"), "x")
	writeFile(t, filepath.Join(linkDir, "other2"), "x")
	expectNoReload(t, w)
	if n := strings.Count(logs.String(), "can't watch"); n > 1 {
		t.Errorf("watch error logged %d times, want once: %s", n, logs.String())
	}

	retarget(t, configPath, filepath.Join(open, "cfg.yaml"))
	expectReload(t, w, "open.localhost")
}
