package internal

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/atas/lazyfwd/internal/config"
)

// Server handles both HTTP and TLS passthrough on a single port
type Server struct {
	config   *config.Config
	manager  *Manager
	listener *MuxListener
	server   *http.Server
	done     chan struct{}
}

// NewServer creates a new unified server
func NewServer(cfg *config.Config, manager *Manager) *Server {
	s := &Server{
		config:  cfg,
		manager: manager,
		done:    make(chan struct{}),
	}
	return s
}

// Start begins listening and handling connections
func (s *Server) Start() error {
	mux, err := NewMuxListener(s.config.HTTP.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.config.HTTP.ListenAddr, err)
	}
	s.listener = mux

	// Create HTTP server using the HTTP-only listener
	s.server = &http.Server{
		Handler:      s,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start HTTP server in goroutine
	go func() {
		err := s.server.Serve(mux.HTTPListener())
		// Ignore expected errors during shutdown
		if err != nil && err != http.ErrServerClosed && !strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Printf("Server listening on %s (HTTP + TLS passthrough)", s.config.HTTP.ListenAddr)

	// Accept loop - route connections based on protocol
	for {
		conn, err := mux.Listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	peekConn := NewPeekConn(conn)

	if peekConn.IsTLS() {
		// Handle as TLS passthrough
		s.handleTLSConnection(peekConn)
	} else {
		// Pass to HTTP server
		select {
		case s.listener.httpConns <- peekConn:
		case <-s.done:
			conn.Close()
		}
	}
}

func (s *Server) handleTLSConnection(conn *PeekConn) {
	defer conn.Close()

	// Set deadline for reading ClientHello
	_ = conn.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Read enough for ClientHello
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		if s.config.Verbose {
			log.Printf("[tls] Error reading ClientHello: %v", err)
		}
		return
	}

	// Clear deadline
	_ = conn.Conn.SetReadDeadline(time.Time{})

	// Extract SNI
	sni, err := extractSNI(buf[:n])
	if err != nil {
		log.Printf("[tls] Failed to extract SNI: %v", err)
		return
	}

	if s.config.Verbose {
		log.Printf("[tls] [%s] New connection", sni)
	}

	// Look up or create tunnel
	tunnel, err := s.manager.GetOrCreateTunnel(sni)
	if err != nil {
		log.Printf("[tls] [%s] Error: %v", sni, err)
		return
	}

	// Ensure tunnel is running
	if !tunnel.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := tunnel.Start(ctx); err != nil {
			cancel()
			log.Printf("[tls] [%s] Failed to start tunnel: %v", sni, err)
			return
		}
		cancel()
	}

	tunnel.Touch()

	// Connect to backend
	backendAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 10*time.Second)
	if err != nil {
		log.Printf("[tls] [%s] Failed to connect to backend: %v", sni, err)
		return
	}
	defer backendConn.Close()

	// Forward the ClientHello
	if _, err := backendConn.Write(buf[:n]); err != nil {
		log.Printf("[tls] [%s] Failed to forward ClientHello: %v", sni, err)
		return
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(backendConn, conn.Conn)
		if tc, ok := backendConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn.Conn, backendConn)
		if tc, ok := conn.Conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// ServeHTTP implements http.Handler for HTTP requests
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if s.config.Verbose {
		log.Printf("[http] [%s] %s %s", host, r.Method, r.URL.Path)
	}

	tunnel, err := s.manager.GetOrCreateTunnel(host)
	if err != nil {
		log.Printf("[http] [%s] Error: %v", host, err)
		http.Error(w, fmt.Sprintf("No service configured for host: %s", host), http.StatusBadGateway)
		return
	}

	if !tunnel.IsRunning() {
		if err := tunnel.Start(r.Context()); err != nil {
			log.Printf("[http] [%s] Failed to start tunnel: %v", host, err)
			http.Error(w, fmt.Sprintf("Failed to start tunnel: %v", err), http.StatusBadGateway)
			return
		}
	}

	tunnel.Touch()

	scheme := tunnel.Scheme()
	targetURL := &url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort()),
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	if scheme == "https" {
		proxy.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = r.Host
		req.Header.Set("X-Forwarded-Proto", scheme)
		req.Header.Set("X-Forwarded-Host", r.Host)
		if r.RemoteAddr != "" {
			req.Header.Set("X-Forwarded-For", strings.Split(r.RemoteAddr, ":")[0])
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Don't log client disconnections - they're normal
		if err == context.Canceled || strings.Contains(err.Error(), "context canceled") {
			return
		}
		log.Printf("[http] [%s] Proxy error: %v", host, err)
	}

	proxy.ServeHTTP(w, r)
}

// Shutdown gracefully stops the server
func (s *Server) Shutdown(ctx context.Context) error {
	// 1. Signal accept loop to stop
	close(s.done)
	// 2. Close listener - this unblocks both the main Accept() and HTTPListener.Accept()
	if s.listener != nil {
		s.listener.Close()
	}
	// 3. Force close HTTP server (don't wait for connections)
	if s.server != nil {
		s.server.Close()
	}
	return nil
}

// extractSNI parses the TLS ClientHello and extracts the SNI extension
func extractSNI(data []byte) (string, error) {
	// TLS record header: type(1) + version(2) + length(2)
	if len(data) < 5 {
		return "", fmt.Errorf("data too short for TLS record")
	}

	// Check if it's a TLS handshake (0x16)
	if data[0] != 0x16 {
		return "", fmt.Errorf("not a TLS handshake record")
	}

	// Record length
	recordLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recordLen {
		return "", fmt.Errorf("incomplete TLS record")
	}

	// Handshake header: type(1) + length(3)
	handshake := data[5:]
	if len(handshake) < 4 {
		return "", fmt.Errorf("data too short for handshake header")
	}

	// Check if it's a ClientHello (0x01)
	if handshake[0] != 0x01 {
		return "", fmt.Errorf("not a ClientHello")
	}

	// Skip handshake header (4 bytes) + version (2) + random (32) = 38 bytes
	pos := 38
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for session ID length")
	}

	// Session ID
	sessionIDLen := int(handshake[pos])
	pos += 1 + sessionIDLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("data too short for cipher suites length")
	}

	// Cipher suites
	cipherSuitesLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2 + cipherSuitesLen
	if len(handshake) < pos+1 {
		return "", fmt.Errorf("data too short for compression methods length")
	}

	// Compression methods
	compressionLen := int(handshake[pos])
	pos += 1 + compressionLen
	if len(handshake) < pos+2 {
		return "", fmt.Errorf("no extensions present")
	}

	// Extensions length
	extensionsLen := int(handshake[pos])<<8 | int(handshake[pos+1])
	pos += 2
	extensionsEnd := pos + extensionsLen

	// Parse extensions
	for pos+4 <= extensionsEnd && pos+4 <= len(handshake) {
		extType := int(handshake[pos])<<8 | int(handshake[pos+1])
		extLen := int(handshake[pos+2])<<8 | int(handshake[pos+3])
		pos += 4

		if pos+extLen > len(handshake) {
			break
		}

		// SNI extension type is 0x0000
		if extType == 0 {
			// SNI extension data: list length (2) + type (1) + name length (2) + name
			extData := handshake[pos : pos+extLen]
			if len(extData) < 5 {
				return "", fmt.Errorf("SNI extension too short")
			}
			// Skip list length (2) + type (1)
			nameLen := int(extData[3])<<8 | int(extData[4])
			if len(extData) < 5+nameLen {
				return "", fmt.Errorf("SNI name truncated")
			}
			return string(extData[5 : 5+nameLen]), nil
		}

		pos += extLen
	}

	return "", fmt.Errorf("SNI extension not found")
}
