package tcpserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/tunnelmgr"
)

type countingTunnel struct {
	tunnelmgr.TunnelHandle
	localPort int
	active    atomic.Int32
}

func (c *countingTunnel) IsRunning() bool             { return true }
func (c *countingTunnel) Start(context.Context) error { return nil }
func (c *countingTunnel) LocalPort() int              { return c.localPort }
func (c *countingTunnel) Touch()                      {}
func (c *countingTunnel) Acquire()                    { c.active.Add(1) }
func (c *countingTunnel) Release()                    { c.active.Add(-1) }
func (c *countingTunnel) ActiveConns() int            { return int(c.active.Load()) }

func TestHandleConnection_CountsConnectionWhileOpen(t *testing.T) {
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
	s := NewServer(testConfig(nil), &mockManager{tunnelToReturn: tun})

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		s.handleConnection(5432, server)
		close(done)
	}()

	// One round trip proves the handler is in the copy
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
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

type seqManager struct {
	mockManager
	tunnels []tunnelmgr.TunnelHandle
	calls   int
}

func (m *seqManager) GetOrCreateTCPTunnel(int) (tunnelmgr.TunnelHandle, error) {
	t := m.tunnels[min(m.calls, len(m.tunnels)-1)]
	m.calls++
	return t, nil
}

// A connection holding a tunnel that failed meanwhile is served by the manager's replacement.
func TestHandleConnection_StaleFailedTunnelUsesReplacement(t *testing.T) {
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

	stale := &staleTunnel{}
	fresh := &countingTunnel{localPort: backend.Addr().(*net.TCPAddr).Port}
	s := NewServer(testConfig(nil), &seqManager{tunnels: []tunnelmgr.TunnelHandle{stale, fresh}})

	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		s.handleConnection(5432, server)
		close(done)
	}()

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
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
