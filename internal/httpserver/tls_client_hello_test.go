package httpserver

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// recordsFrom re-frames the handshake message of a single-record hello into
// records with the given payload sizes; the last record takes the remainder.
func recordsFrom(hello []byte, sizes ...int) []byte {
	payload := hello[5:]
	var out []byte
	appendRecord := func(p []byte) {
		out = append(out, hello[0], hello[1], hello[2], byte(len(p)>>8), byte(len(p)))
		out = append(out, p...)
	}
	for _, size := range sizes {
		appendRecord(payload[:size])
		payload = payload[size:]
	}
	appendRecord(payload)
	return out
}

func TestExtractSNI_MultipleRecords(t *testing.T) {
	hello := generateClientHello("split.localhost")

	for _, sizes := range [][]int{{1}, {3}, {4}, {20}, {1, 1, 1, 1, 30}} {
		sni, err := extractSNI(recordsFrom(hello, sizes...))
		if err != nil {
			t.Errorf("sizes %v: %v", sizes, err)
			continue
		}
		if sni != "split.localhost" {
			t.Errorf("sizes %v: sni = %q", sizes, sni)
		}
	}
}

func TestExtractSNI_NonHandshakeRecordInsideHello(t *testing.T) {
	data := recordsFrom(generateClientHello("split.localhost"), 20)
	data[5+20] = 0x17 // the second record is application data

	if _, err := extractSNI(data); err == nil || errors.Is(err, errIncompleteClientHello) {
		t.Errorf("err = %v, want a malformed hello error", err)
	}
}

// passthroughBackend accepts one connection and reports what it received once
// the client side is done.
func passthroughBackend(t *testing.T) (port int, received <-chan []byte) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ch := make(chan []byte, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		data, _ := io.ReadAll(conn)
		ch <- data
	}()
	return listenerPort(l), ch
}

// dialThroughServer connects a client to server.handleConnection over real TCP,
// going through the same first-byte peek as production.
func dialThroughServer(t *testing.T, server *Server) net.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	serverSide := <-accepted
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleConnection(serverSide)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("handleConnection did not return")
		}
	})
	return client
}

func newTLSTestServer(port int) (*Server, *tlsMockManager) {
	mgr := &tlsMockManager{tunnel: &tlsMockTunnel{running: true, localPort: port}}
	return NewServer(testHTTPConfig(), mgr), mgr
}

func readBackend(t *testing.T, received <-chan []byte) []byte {
	t.Helper()
	select {
	case data := <-received:
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("backend received nothing")
		return nil
	}
}

// paddingProtos returns an ALPN list that makes a crypto/tls hello about size bytes larger.
func paddingProtos(size int) []string {
	var protos []string
	for ; size > 0; size -= 101 {
		protos = append(protos, strings.Repeat("p", 100))
	}
	return protos
}

// A real crypto/tls hello padded with ALPN protocols, past the 4096-byte read
// buffer and past the 16384-byte limit of a single record.
func TestHandleConnection_LargeRealClientHello(t *testing.T) {
	for _, padding := range []int{7000, 20000} {
		port, received := passthroughBackend(t)
		server, mgr := newTLSTestServer(port)
		client := dialThroughServer(t, server)

		tlsClient := tls.Client(client, &tls.Config{ServerName: "big.localhost", NextProtos: paddingProtos(padding), InsecureSkipVerify: true})
		_ = client.SetDeadline(time.Now().Add(200 * time.Millisecond))
		_ = tlsClient.Handshake() // the backend never answers
		_ = client.(*net.TCPConn).CloseWrite()

		data := readBackend(t, received)
		if len(data) < padding {
			t.Errorf("padding %d: backend got only %d bytes", padding, len(data))
		}
		sni, err := extractSNI(data)
		if err != nil || sni != "big.localhost" {
			t.Errorf("padding %d: backend hello gave sni %q, err %v", padding, sni, err)
		}
		if calls := mgr.GetCalls(); len(calls) != 1 || calls[0] != "big.localhost" {
			t.Errorf("padding %d: GetOrCreateTunnel calls = %v", padding, calls)
		}
	}
}

// The error page replays the hello into its own TLS handshake, which has to name
// the host the client asked for rather than failing to parse the hello.
func TestHandleConnection_LargeClientHelloGetsErrorPageForHost(t *testing.T) {
	mgr := &tlsMockManager{err: errors.New("no route configured")}
	client := dialThroughServer(t, NewServer(testHTTPConfig(), mgr))

	tlsClient := tls.Client(client, &tls.Config{ServerName: "big.localhost", NextProtos: paddingProtos(7000), InsecureSkipVerify: true})
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("handshake with the error page failed: %v", err)
	}

	if name := tlsClient.ConnectionState().PeerCertificates[0].Subject.CommonName; name != "big.localhost" {
		t.Errorf("error page certificate is for %q, want big.localhost", name)
	}
}

// Whatever way the hello arrives, the backend must get the same bytes, including
// anything the client sent right after the hello.
func TestHandleConnection_SplitClientHello(t *testing.T) {
	hello := generateClientHello("split.localhost")
	multi := recordsFrom(hello, 20)

	tests := []struct {
		name   string
		writes [][]byte
	}{
		{"split inside the record header", [][]byte{hello[:3], hello[3:]}},
		{"split inside the record", [][]byte{hello[:20], hello[20:]}},
		{"two records in one write", [][]byte{multi}},
		{"two records in two writes", [][]byte{multi[:25], multi[25:]}},
		{"hello then early data", [][]byte{append(bytes.Clone(hello), "early data"...)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, received := passthroughBackend(t)
			server, mgr := newTLSTestServer(port)
			client := dialThroughServer(t, server)

			var want []byte
			for _, write := range tt.writes {
				want = append(want, write...)
				if _, err := client.Write(write); err != nil {
					t.Fatal(err)
				}
				time.Sleep(30 * time.Millisecond)
			}
			_ = client.(*net.TCPConn).CloseWrite()

			if got := readBackend(t, received); !bytes.Equal(got, want) {
				t.Errorf("backend got %d bytes that differ from the %d sent", len(got), len(want))
			}
			if calls := mgr.GetCalls(); len(calls) != 1 || calls[0] != "split.localhost" {
				t.Errorf("GetOrCreateTunnel calls = %v", calls)
			}
		})
	}
}

// A client claiming a huge hello must not be buffered without limit.
func TestReadClientHello_SizeLimit(t *testing.T) {
	client, serverSide := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = serverSide.Close() }()

	go func() {
		header := []byte{0x16, 0x03, 0x01, 0x40, 0x00} // a full 16384-byte record
		first := append(bytes.Clone(header), 0x01, 0xff, 0xff, 0xff)
		first = append(first, make([]byte, 16384-4)...)
		if _, err := client.Write(first); err != nil {
			return
		}
		for {
			if _, err := client.Write(append(bytes.Clone(header), make([]byte, 16384)...)); err != nil {
				return
			}
		}
	}()

	_ = serverSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := readClientHello(newPeekConn(serverSide))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("hit the deadline instead of the size limit: %v", err)
	}
}

func TestReadClientHello_StopsAtDeadline(t *testing.T) {
	client, serverSide := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = serverSide.Close() }()

	go func() { _, _ = client.Write(generateClientHello("slow.localhost")[:20]) }()

	_ = serverSide.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := readClientHello(newPeekConn(serverSide)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("err = %v, want the read deadline", err)
	}
}
