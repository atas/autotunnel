package httpserver

import (
	"bufio"
	"net"
)

// peekConn wraps a net.Conn with a buffered reader for peeking
type peekConn struct {
	net.Conn
	reader *bufio.Reader
}

// newPeekConn creates a new peekConn
func newPeekConn(conn net.Conn) *peekConn {
	return &peekConn{
		Conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// Read reads from the buffered reader
func (c *peekConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// Peek returns the next n bytes without advancing the reader
func (c *peekConn) Peek(n int) ([]byte, error) {
	return c.reader.Peek(n)
}

// isTLS checks if the connection starts with a TLS handshake
func (c *peekConn) isTLS() bool {
	// Peek at first byte - TLS handshake starts with 0x16
	b, err := c.reader.Peek(1)
	if err != nil {
		return false
	}
	return b[0] == 0x16
}

// muxListener is a listener that separates TLS and HTTP connections
type muxListener struct {
	net.Listener
	httpConns chan net.Conn
	done      chan struct{}
}

// newMuxListener creates a multiplexing listener
func newMuxListener(addr string) (*muxListener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &muxListener{
		Listener:  l,
		httpConns: make(chan net.Conn, 256),
		done:      make(chan struct{}),
	}, nil
}

// httpListener returns a listener that only accepts HTTP connections
func (m *muxListener) httpListener() net.Listener {
	return &httpOnlyListener{mux: m}
}

// Close closes the underlying listener
func (m *muxListener) Close() error {
	close(m.done)
	return m.Listener.Close()
}

// httpOnlyListener wraps muxListener to implement net.Listener for HTTP
type httpOnlyListener struct {
	mux *muxListener
}

func (h *httpOnlyListener) Accept() (net.Conn, error) {
	select {
	case conn := <-h.mux.httpConns:
		return conn, nil
	case <-h.mux.done:
		return nil, net.ErrClosed
	}
}

func (h *httpOnlyListener) Close() error {
	return nil // Closed by muxListener
}

func (h *httpOnlyListener) Addr() net.Addr {
	return h.mux.Listener.Addr()
}
