package internal

import (
	"bufio"
	"net"
)

// PeekConn wraps a net.Conn with a buffered reader for peeking
type PeekConn struct {
	net.Conn
	reader *bufio.Reader
}

// NewPeekConn creates a new PeekConn
func NewPeekConn(conn net.Conn) *PeekConn {
	return &PeekConn{
		Conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// Read reads from the buffered reader
func (c *PeekConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// Peek returns the next n bytes without advancing the reader
func (c *PeekConn) Peek(n int) ([]byte, error) {
	return c.reader.Peek(n)
}

// IsTLS checks if the connection starts with a TLS handshake
func (c *PeekConn) IsTLS() bool {
	// Peek at first byte - TLS handshake starts with 0x16
	b, err := c.reader.Peek(1)
	if err != nil {
		return false
	}
	return b[0] == 0x16
}

// MuxListener is a listener that separates TLS and HTTP connections
type MuxListener struct {
	net.Listener
	httpConns chan net.Conn
	done      chan struct{}
}

// NewMuxListener creates a multiplexing listener
func NewMuxListener(addr string) (*MuxListener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &MuxListener{
		Listener:  l,
		httpConns: make(chan net.Conn, 256),
		done:      make(chan struct{}),
	}, nil
}

// HTTPListener returns a listener that only accepts HTTP connections
func (m *MuxListener) HTTPListener() net.Listener {
	return &httpListener{mux: m}
}

// Close closes the underlying listener
func (m *MuxListener) Close() error {
	close(m.done)
	return m.Listener.Close()
}

// httpListener wraps MuxListener to implement net.Listener for HTTP
type httpListener struct {
	mux *MuxListener
}

func (h *httpListener) Accept() (net.Conn, error) {
	select {
	case conn := <-h.mux.httpConns:
		return conn, nil
	case <-h.mux.done:
		return nil, net.ErrClosed
	}
}

func (h *httpListener) Close() error {
	return nil // Closed by MuxListener
}

func (h *httpListener) Addr() net.Addr {
	return h.mux.Listener.Addr()
}
