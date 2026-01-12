package httpserver

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// tlsErrorType represents different error types for TLS connections.
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

// sendTLSErrorPage completes a TLS handshake and sends an HTTP error page.
// It uses the stored ClientHello bytes to properly complete the handshake.
func (s *Server) sendTLSErrorPage(conn net.Conn, clientHello []byte, hostname string, errType tlsErrorType, errMsg string) {
	// If we don't have the cert provider, just close
	if s.tlsErrorCertProvider == nil {
		return
	}

	// If no hostname was extracted, use a generic one
	if hostname == "" {
		hostname = "unknown.localhost"
	}

	// Get or generate certificate for this hostname
	cert, err := s.tlsErrorCertProvider.GetCertificate(hostname)
	if err != nil {
		if s.config.Verbose {
			log.Printf("[tls] [%s] Failed to generate error cert: %v", hostname, err)
		}
		return
	}

	// Create TLS config with our certificate
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create a wrapper that replays the ClientHello
	replayConn := &replayConn{
		Conn:    conn,
		initial: clientHello,
	}

	// Perform TLS handshake
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

	// Generate and send error page
	statusCode := errType.statusCode()
	body := renderErrorPage(statusCode, hostname, errMsg)

	// Write HTTP response
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

// replayConn wraps a connection and replays initial bytes on first read.
// This is used to replay the ClientHello bytes during the TLS handshake.
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
