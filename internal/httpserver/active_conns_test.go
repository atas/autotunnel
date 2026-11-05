package httpserver

import (
	"bufio"
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

func (c *countingTunnel) IsRunning() bool  { return true }
func (c *countingTunnel) LocalPort() int   { return c.localPort }
func (c *countingTunnel) Scheme() string   { return "http" }
func (c *countingTunnel) Touch()           {}
func (c *countingTunnel) Acquire()         { c.active.Add(1) }
func (c *countingTunnel) Release()         { c.active.Add(-1) }
func (c *countingTunnel) ActiveConns() int { return int(c.active.Load()) }

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
