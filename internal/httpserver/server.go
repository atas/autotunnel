package httpserver

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/atas/lazyfwd/internal/config"
	"github.com/atas/lazyfwd/internal/tunnelmgr"
)

// Manager provides tunnel lifecycle management
type Manager interface {
	GetOrCreateTunnel(hostname string, scheme string) (tunnelmgr.TunnelHandle, error)
}

// Server handles both HTTP and TLS passthrough on a single port
type Server struct {
	config               *config.Config
	manager              Manager
	listener             *muxListener
	server               *http.Server
	done                 chan struct{}
	tlsErrorCertProvider *tlsErrorCertProvider
}

// NewServer creates a new unified server
func NewServer(cfg *config.Config, mgr Manager) *Server {
	// Initialize TLS error cert provider (ignore errors - will just not show TLS error pages if it fails)
	certProvider, _ := newTLSErrorCertProvider()

	return &Server{
		config:               cfg,
		manager:              mgr,
		done:                 make(chan struct{}),
		tlsErrorCertProvider: certProvider,
	}
}

// Start begins listening and handling connections
func (s *Server) Start() error {
	mux, err := newMuxListener(s.config.HTTP.ListenAddr)
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
		err := s.server.Serve(mux.httpListener())
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
	peekConn := newPeekConn(conn)

	if peekConn.isTLS() {
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

// Shutdown gracefully stops the server
func (s *Server) Shutdown(ctx context.Context) error {
	// 1. Signal accept loop to stop
	close(s.done)
	// 2. Close listener - this unblocks both the main Accept() and httpListener.Accept()
	if s.listener != nil {
		s.listener.Close()
	}
	// 3. Force close HTTP server (don't wait for connections)
	if s.server != nil {
		s.server.Close()
	}
	return nil
}
