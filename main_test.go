package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/watcher"
)

// testConfig returns a config on the given ports. The kubeconfig does not exist, so nothing here
// can reach a cluster, even if a connection comes in on a route.
func testConfig(t *testing.T, httpPort, tcpPort int) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: autotunnel/v1
http:
  listen: "127.0.0.1:%d"
  idle_timeout: 60m
  k8s:
    kubeconfig: %s
`, httpPort, filepath.Join(t.TempDir(), "no-kubeconfig"))
	if tcpPort != 0 {
		content += fmt.Sprintf(`tcp:
  idle_timeout: 60m
  k8s:
    routes:
      %d:
        context: test
        namespace: default
        service: test
        port: 5432
`, tcpPort)
	}
	return content
}

func freePort(t *testing.T) int {
	t.Helper()
	port, holder := holdPort(t)
	_ = holder.Close()
	return port
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

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
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

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

type runningApp struct {
	configPath string
	watcher    *watcher.ConfigWatcher
	done       chan error
}

// startRun runs the restart loop on the config, with a watcher, and stops it when the test ends.
func startRun(t *testing.T, content string) *runningApp {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "cfg.yaml")
	writeConfig(t, configPath, content)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	w, err := watcher.NewConfigWatcher(configPath, cfg, false)
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}
	w.Start()

	app := &runningApp{configPath: configPath, watcher: w, done: make(chan error, 1)}
	sigChan := make(chan os.Signal, 1)
	go func() { app.done <- run(configPath, cfg, w, sigChan) }()

	t.Cleanup(func() {
		sigChan <- syscall.SIGTERM
		select {
		case err := <-app.done:
			if err != nil {
				t.Errorf("run returned %v on SIGTERM", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("run did not stop on signal")
		}
		w.Stop()
	})
	return app
}

// expectAlive fails if run has returned.
func (a *runningApp) expectAlive(t *testing.T) {
	t.Helper()
	select {
	case err := <-a.done:
		a.done <- err
		t.Fatalf("run exited: %v", err)
	default:
	}
}

// waitFor polls until cond holds, failing early if run exits.
func (a *runningApp) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		a.expectAlive(t)
		if time.Now().After(deadline) {
			t.Fatalf("Timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// getOnConn sends a request on an open connection, which only works while the instance that
// accepted it is still up.
func getOnConn(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: unknown.localhost\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestRun_BusyPortAtFirstStartExits(t *testing.T) {
	tests := []struct {
		name    string
		content func(busy int) string
	}{
		{"http", func(busy int) string { return testConfig(t, busy, 0) }},
		{"tcp", func(busy int) string { return testConfig(t, freePort(t), busy) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			busy, _ := holdPort(t)
			configPath := filepath.Join(t.TempDir(), "cfg.yaml")
			writeConfig(t, configPath, tt.content(busy))
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				t.Fatalf("Failed to load config: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- run(configPath, cfg, nil, make(chan os.Signal)) }()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(busy)) {
					t.Errorf("Expected an error naming port %d, got: %v", busy, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Expected run to exit")
			}
		})
	}
}

// The docs used to say port changes need a manual restart. They do not.
func TestRun_ReloadMovesToNewPorts(t *testing.T) {
	httpPort, tcpPort := freePort(t), freePort(t)
	app := startRun(t, testConfig(t, httpPort, tcpPort))
	app.waitFor(t, "first start", func() bool { return isListening(httpPort) && isListening(tcpPort) })

	newHTTP, newTCP := freePort(t), freePort(t)
	writeConfig(t, app.configPath, testConfig(t, newHTTP, newTCP))
	app.waitFor(t, "new ports", func() bool { return isListening(newHTTP) && isListening(newTCP) })
	app.waitFor(t, "old ports to close", func() bool { return !isListening(httpPort) && !isListening(tcpPort) })
}

func TestRun_ReloadToBusyPortKeepsRunning(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, httpPort, tcpPort, busy int) string
	}{
		{"new tcp route", func(t *testing.T, httpPort, tcpPort, busy int) string { return testConfig(t, httpPort, busy) }},
		{"new http port", func(t *testing.T, httpPort, tcpPort, busy int) string { return testConfig(t, busy, tcpPort) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLog(t)
			httpPort, tcpPort := freePort(t), freePort(t)
			app := startRun(t, testConfig(t, httpPort, tcpPort))
			app.waitFor(t, "first start", func() bool { return isListening(httpPort) && isListening(tcpPort) })

			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := getOnConn(conn); err != nil {
				t.Fatalf("Request before the reload failed: %v", err)
			}

			busy, holder := holdPort(t)
			edited := tt.edit(t, httpPort, tcpPort, busy)
			writeConfig(t, app.configPath, edited)
			app.waitFor(t, "failure to be logged", func() bool { return strings.Contains(logs.String(), "keeping current config") })

			if !isListening(httpPort) || !isListening(tcpPort) {
				t.Error("Expected the old ports to keep listening")
			}
			if err := getOnConn(conn); err != nil {
				t.Errorf("Expected the old instance to stay up without a restart: %v", err)
			}

			// Once the port is free, saving again applies the edit
			_ = holder.Close()
			writeConfig(t, app.configPath, edited)
			app.waitFor(t, "new port", func() bool { return isListening(busy) })
		})
	}
}

// The port check before a restart can lose a race, or not apply at all, so a failed start after
// the old instance has stopped must bring the old config back too.
func TestRun_FailedRestartRestoresOldConfig(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, httpPort, tcpPort, busy int) string
	}{
		{"new tcp route", func(t *testing.T, httpPort, tcpPort, busy int) string { return testConfig(t, httpPort, busy) }},
		{"new http port", func(t *testing.T, httpPort, tcpPort, busy int) string { return testConfig(t, busy, tcpPort) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLog(t)
			httpPort, tcpPort := freePort(t), freePort(t)
			app := startRun(t, testConfig(t, httpPort, tcpPort))
			app.waitFor(t, "first start", func() bool { return isListening(httpPort) && isListening(tcpPort) })
			original := app.watcher.GetConfig()

			busy, _ := holdPort(t)
			hand := func(content string) {
				t.Helper()
				path := filepath.Join(t.TempDir(), "next.yaml")
				writeConfig(t, path, content)
				next, err := config.LoadConfig(path)
				if err != nil {
					t.Fatalf("Failed to load config: %v", err)
				}
				// Go around the watcher's port check, as if the port got taken right after it
				app.watcher.RestoreConfig(app.watcher.GetConfig(), next)
				app.watcher.ReloadChan <- struct{}{}
			}

			hand(tt.edit(t, httpPort, tcpPort, busy))
			app.waitFor(t, "old config", func() bool {
				return strings.Contains(logs.String(), "restoring previous config") && app.watcher.GetConfig() == original
			})
			app.waitFor(t, "old ports", func() bool { return isListening(httpPort) && isListening(tcpPort) })

			// A later good config becomes what the next failure goes back to
			newHTTP := freePort(t)
			writeConfig(t, app.configPath, testConfig(t, newHTTP, tcpPort))
			app.waitFor(t, "new port", func() bool { return isListening(newHTTP) })
			good := app.watcher.GetConfig()

			hand(tt.edit(t, newHTTP, tcpPort, busy))
			app.waitFor(t, "newer good config", func() bool { return app.watcher.GetConfig() == good })
			app.waitFor(t, "newer good ports", func() bool { return isListening(newHTTP) && isListening(tcpPort) })
			app.expectAlive(t)
		})
	}
}

// A reload can already be waiting when an instance starts, so it is shut down at once. Its HTTP
// listener must be closed by then, or the late bind holds the port and every next start fails.
func TestRun_ReloadWaitingAtStartFreesPorts(t *testing.T) {
	httpPort, tcpPort := freePort(t), freePort(t)
	app := startRun(t, testConfig(t, httpPort, tcpPort))
	app.waitFor(t, "first start", func() bool { return isListening(httpPort) && isListening(tcpPort) })

	// Each send blocks until the loop takes the previous one, so the next start finds one waiting
	for i := 0; i < 30; i++ {
		app.watcher.ReloadChan <- struct{}{}
	}
	app.waitFor(t, "ports after restarts", func() bool { return isListening(httpPort) && isListening(tcpPort) })
	app.expectAlive(t)
}
