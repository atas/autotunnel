package httpserver

import (
	"bufio"
	"net"
)

type peekConn struct {
	net.Conn
	reader *bufio.Reader
}

func newPeekConn(conn net.Conn) *peekConn {
	return &peekConn{
		Conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

func (c *peekConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

func (c *peekConn) Peek(n int) ([]byte, error) {
	return c.reader.Peek(n)
}

func (c *peekConn) isTLS() bool {
	// 0x16 = TLS handshake record type
	b, err := c.reader.Peek(1)
	if err != nil {
		return false
	}
	return b[0] == 0x16
}

// muxListener routes connections to either TLS passthrough or HTTP based on first byte
type muxListener struct {
	net.Listener
	httpConns chan net.Conn
	done      chan struct{}
}

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

func (m *muxListener) httpListener() net.Listener {
	return &httpOnlyListener{mux: m}
}

func (m *muxListener) Close() error {
	close(m.done)
	return m.Listener.Close()
}

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
	return nil // muxListener handles this
}

func (h *httpOnlyListener) Addr() net.Addr {
	return h.mux.Listener.Addr()
}
