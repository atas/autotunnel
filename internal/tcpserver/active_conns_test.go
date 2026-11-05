package tcpserver

import (
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

func (c *countingTunnel) IsRunning() bool  { return true }
func (c *countingTunnel) LocalPort() int   { return c.localPort }
func (c *countingTunnel) Touch()           {}
func (c *countingTunnel) Acquire()         { c.active.Add(1) }
func (c *countingTunnel) Release()         { c.active.Add(-1) }
func (c *countingTunnel) ActiveConns() int { return int(c.active.Load()) }

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
