package httpserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/tunnelmgr"
)

func (m *mockTunnel) Acquire()         {}
func (m *mockTunnel) Release()         {}
func (m *mockTunnel) ActiveConns() int { return 0 }

func (m *retryMockTunnel) Acquire()         {}
func (m *retryMockTunnel) Release()         {}
func (m *retryMockTunnel) ActiveConns() int { return 0 }

func (m *tlsMockTunnel) Acquire()         {}
func (m *tlsMockTunnel) Release()         {}
func (m *tlsMockTunnel) ActiveConns() int { return 0 }

type countingTunnel struct {
	tunnelmgr.TunnelHandle
	localPort int
	active    atomic.Int32
}

func (c *countingTunnel) IsRunning() bool             { return true }
func (c *countingTunnel) Start(context.Context) error { return nil }
func (c *countingTunnel) LocalPort() int              { return c.localPort }
func (c *countingTunnel) Scheme() string              { return "http" }
func (c *countingTunnel) Touch()                      {}
func (c *countingTunnel) Acquire()                    { c.active.Add(1) }
func (c *countingTunnel) Release()                    { c.active.Add(-1) }
func (c *countingTunnel) ActiveConns() int            { return int(c.active.Load()) }

func waitForActiveConns(t *testing.T, tun *countingTunnel, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for tun.ActiveConns() != want {
		if time.Now().After(deadline) {
			t.Fatalf("ActiveConns = %d, want %d", tun.ActiveConns(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandleTLSConnection_CountsConnectionWhileOpen(t *testing.T) {
	clientHello := generateClientHello("test.localhost")

	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	tun := &countingTunnel{localPort: backend.Addr().(*net.TCPAddr).Port}
	s := NewServer(testHTTPConfig(), &mockManager{tunnel: tun})

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		s.handleTLSConnection(newPeekConn(server))
		close(done)
	}()

	// The echoed ClientHello proves the handler is in the copy
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write(clientHello); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, len(clientHello))); err != nil {
		t.Fatal(err)
	}
	if got := tun.ActiveConns(); got != 1 {
		t.Errorf("ActiveConns during the connection = %d, want 1", got)
	}

	client.Close()
	<-done

	if got := tun.ActiveConns(); got != 0 {
		t.Errorf("ActiveConns after the connection = %d, want 0", got)
	}
}

// A WebSocket lives inside one ReverseProxy.ServeHTTP call, however long it stays open.
func TestServeHTTP_CountsUpgradedConnectionWhileOpen(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusSwitchingProtocols)
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	}))
	defer backend.Close()

	tun := &countingTunnel{localPort: backend.Listener.Addr().(*net.TCPAddr).Port}
	front := httptest.NewServer(NewServer(testHTTPConfig(), &mockManager{tunnel: tun}))
	defer front.Close()

	client, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprint(client, "GET /ws HTTP/1.1\r\nHost: test.localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(client)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if got := tun.ActiveConns(); got != 1 {
		t.Errorf("ActiveConns during the WebSocket = %d, want 1", got)
	}

	client.Close()
	waitForActiveConns(t, tun, 0)
}

// staleTunnel failed after the manager handed it out, as when another request marked it failed.
type staleTunnel struct {
	tunnelmgr.TunnelHandle
	active atomic.Int32
}

func (s *staleTunnel) IsRunning() bool { return false }
func (s *staleTunnel) Start(context.Context) error {
	return fmt.Errorf("%w: port-forward died", tunnelmgr.ErrTunnelFailed)
}
func (s *staleTunnel) Acquire() { s.active.Add(1) }
func (s *staleTunnel) Release() { s.active.Add(-1) }

// seqManager hands out its tunnels in order, the last one from then on.
type seqManager struct {
	tunnels []tunnelmgr.TunnelHandle
	calls   int
}

func (m *seqManager) GetOrCreateTunnel(string, string) (tunnelmgr.TunnelHandle, error) {
	t := m.tunnels[min(m.calls, len(m.tunnels)-1)]
	m.calls++
	return t, nil
}

func echoBackend(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// A request holding a tunnel that failed meanwhile is served by the manager's replacement.
func TestServeHTTP_StaleFailedTunnelUsesReplacement(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	fresh := &countingTunnel{localPort: backend.Listener.Addr().(*net.TCPAddr).Port}
	mgr := &seqManager{tunnels: []tunnelmgr.TunnelHandle{&staleTunnel{}, fresh}}
	front := httptest.NewServer(NewServer(testHTTPConfig(), mgr))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if mgr.calls != 2 {
		t.Errorf("GetOrCreateTunnel calls = %d, want 2", mgr.calls)
	}
}

func TestHandleTLSConnection_StaleFailedTunnelUsesReplacement(t *testing.T) {
	clientHello := generateClientHello("test.localhost")
	stale := &staleTunnel{}
	fresh := &countingTunnel{localPort: echoBackend(t)}
	s := NewServer(testHTTPConfig(), &seqManager{tunnels: []tunnelmgr.TunnelHandle{stale, fresh}})

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		s.handleTLSConnection(newPeekConn(server))
		close(done)
	}()

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write(clientHello); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, len(clientHello))); err != nil {
		t.Fatalf("connection not served by the replacement: %v", err)
	}
	if got := fresh.ActiveConns(); got != 1 {
		t.Errorf("replacement ActiveConns = %d, want 1", got)
	}
	if got := stale.active.Load(); got != 0 {
		t.Errorf("failed tunnel still counted: %d", got)
	}

	client.Close()
	<-done
	if got := fresh.ActiveConns(); got != 0 {
		t.Errorf("replacement ActiveConns after = %d, want 0", got)
	}
}
