package httpserver

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

type tlsErrorType int

const (
	tlsErrorSNIExtraction tlsErrorType = iota
	tlsErrorRouteNotFound
	tlsErrorTunnelStartup
	tlsErrorBackendConnection
	tlsErrorForwarding
)

func (e tlsErrorType) statusCode() int {
	switch e {
	case tlsErrorRouteNotFound:
		return http.StatusNotFound
	case tlsErrorSNIExtraction:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// sendTLSErrorPage generates a self-signed cert, completes TLS handshake,
// and returns an HTTP error. Without this, browsers just show "connection reset".
func (s *Server) sendTLSErrorPage(conn net.Conn, clientHello []byte, hostname string, errType tlsErrorType, errMsg string) {
	if s.tlsErrorCertProvider == nil {
		return
	}

	if hostname == "" {
		hostname = "unknown.localhost"
	}

	cert, err := s.tlsErrorCertProvider.GetCertificate(hostname)
	if err != nil {
		if s.config.Verbose {
			log.Printf("[tls] [%s] Failed to generate error cert: %v", hostname, err)
		}
		return
	}
	if cert == nil {
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}

	// we already consumed the ClientHello, need to replay it for TLS handshake
	replayConn := &replayConn{
		Conn:    conn,
		initial: clientHello,
	}

	tlsConn := tls.Server(replayConn, tlsConfig)
	defer tlsConn.Close()

	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}

	if err := tlsConn.Handshake(); err != nil {
		if s.config.Verbose {
			log.Printf("[tls] [%s] Error page handshake failed: %v", hostname, err)
		}
		return
	}

	statusCode := errType.statusCode()
	body := renderErrorPage(statusCode, hostname, errMsg)

	response := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n"+
			"\r\n",
		statusCode,
		http.StatusText(statusCode),
		len(body),
	)

	_, _ = tlsConn.Write([]byte(response))
	_, _ = tlsConn.Write(body)
}

type replayConn struct {
	net.Conn
	initial  []byte
	replayed bool
}

func (r *replayConn) Read(b []byte) (int, error) {
	if !r.replayed && len(r.initial) > 0 {
		n := copy(b, r.initial)
		if n >= len(r.initial) {
			r.replayed = true
			r.initial = nil
		} else {
			r.initial = r.initial[n:]
		}
		return n, nil
	}
	return r.Conn.Read(b)
}
