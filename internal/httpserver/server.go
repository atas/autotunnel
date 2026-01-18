package httpserver

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/tunnelmgr"
)

type Manager interface {
	GetOrCreateTunnel(hostname string, scheme string) (tunnelmgr.TunnelHandle, error)
}

type Server struct {
	config               *config.Config
	manager              Manager
	listener             *muxListener
	server               *http.Server
	done                 chan struct{}
	tlsErrorCertProvider *tlsErrorCertProvider
}

func NewServer(cfg *config.Config, mgr Manager) *Server {
	// cert generation can fail (rare) - we just won't show TLS error pages then
	certProvider, _ := newTLSErrorCertProvider()

	return &Server{
		config:               cfg,
		manager:              mgr,
		done:                 make(chan struct{}),
		tlsErrorCertProvider: certProvider,
	}
}

func (s *Server) Start() error {
	mux, err := newMuxListener(s.config.HTTP.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.config.HTTP.ListenAddr, err)
	}
	s.listener = mux

	s.server = &http.Server{
		Handler:      s,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		err := s.server.Serve(mux.httpListener())
		// these errors are expected during shutdown, don't spam the logs
		if err != nil && err != http.ErrServerClosed && !strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Printf("Server listening on %s (HTTP + TLS passthrough)", s.config.HTTP.ListenAddr)

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
		s.handleTLSConnection(peekConn)
	} else {
		select {
		case s.listener.httpConns <- peekConn:
		case <-s.done:
			_ = conn.Close()
		}
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	close(s.done)
	// Close listener first - unblocks Accept() calls
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	return nil
}
