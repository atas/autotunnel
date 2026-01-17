package tcpserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/atas/autotunnel/internal/config"
)

type listenerType int

const (
	listenerTypeRoute listenerType = iota // direct port-forward
	listenerTypeSocat                     // jump-host via exec+socat
)

type Server struct {
	config  *config.Config
	manager Manager
	verbose bool

	mu        sync.RWMutex
	listeners map[int]*portListener

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type portListener struct {
	port         int
	listenerType listenerType
	listener     net.Listener
	stopChan     chan struct{}
}

func NewServer(cfg *config.Config, mgr Manager) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:    cfg,
		manager:   mgr,
		verbose:   cfg.Verbose,
		listeners: make(map[int]*portListener),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (s *Server) Start() error {
	for port := range s.config.TCP.K8s.Routes {
		if err := s.startListener(port, listenerTypeRoute); err != nil {
			s.Shutdown()
			return err
		}
	}

	for port := range s.config.TCP.K8s.Socat {
		if err := s.startListener(port, listenerTypeSocat); err != nil {
			s.Shutdown()
			return err
		}
	}

	return nil
}

func (s *Server) startListener(port int, lt listenerType) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on TCP port %d: %w", port, err)
	}

	pl := &portListener{
		port:         port,
		listenerType: lt,
		listener:     listener,
		stopChan:     make(chan struct{}),
	}

	s.mu.Lock()
	s.listeners[port] = pl
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(pl)

	typeStr := "route"
	if lt == listenerTypeSocat {
		typeStr = "socat"
	}
	log.Printf("TCP listener started on %s (%s)", addr, typeStr)
	return nil
}

func (s *Server) acceptLoop(pl *portListener) {
	defer s.wg.Done()

	for {
		conn, err := pl.listener.Accept()
		if err != nil {
			select {
			case <-pl.stopChan:
				return
			case <-s.ctx.Done():
				return
			default:
				if s.verbose {
					log.Printf("[tcp:%d] Accept error: %v", pl.port, err)
				}
				continue
			}
		}

		if pl.listenerType == listenerTypeSocat {
			go s.handleSocatConnection(pl.port, conn)
		} else {
			go s.handleConnection(pl.port, conn)
		}
	}
}

func (s *Server) handleConnection(localPort int, conn net.Conn) {
	defer conn.Close()

	// Get or create tunnel for this port
	tunnel, err := s.manager.GetOrCreateTCPTunnel(localPort)
	if err != nil {
		log.Printf("[tcp:%d] Failed to get tunnel: %v", localPort, err)
		return
	}

	// Ensure tunnel is started
	if !tunnel.IsRunning() {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		if err := tunnel.Start(ctx); err != nil {
			cancel()
			log.Printf("[tcp:%d] Failed to start tunnel: %v", localPort, err)
			return
		}
		cancel()
	}

	// Connect to tunnel's local port
	backendAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort())
	backend, err := net.DialTimeout("tcp", backendAddr, 10*time.Second)
	if err != nil {
		log.Printf("[tcp:%d] Failed to connect to backend %s: %v", localPort, backendAddr, err)
		return
	}
	defer backend.Close()

	tunnel.Touch()

	if s.verbose {
		log.Printf("[tcp:%d] Connection established -> backend port %d", localPort, tunnel.LocalPort())
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, conn)
		if tc, ok := backend.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, backend)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()

	if s.verbose {
		log.Printf("[tcp:%d] Connection closed", localPort)
	}
}

func (s *Server) handleSocatConnection(localPort int, conn net.Conn) {
	s.mu.RLock()
	route, exists := s.config.TCP.K8s.Socat[localPort]
	kubeconfigs := s.config.TCP.K8s.ResolvedKubeconfigs
	s.mu.RUnlock()

	if !exists {
		log.Printf("[socat:%d] No route configured", localPort)
		conn.Close()
		return
	}

	clientset, restConfig, err := s.manager.GetClientForContext(kubeconfigs, route.Context)
	if err != nil {
		log.Printf("[socat:%d] Failed to get K8s client: %v", localPort, err)
		conn.Close()
		return
	}

	handler := NewJumpHandler(route, kubeconfigs, clientset, restConfig, s.verbose)
	if err := handler.HandleConnection(s.ctx, conn, localPort); err != nil {
		log.Printf("[socat:%d] Connection error: %v", localPort, err)
	}
}

func (s *Server) Shutdown() {
	s.cancel()

	s.mu.Lock()
	for port, pl := range s.listeners {
		close(pl.stopChan)
		pl.listener.Close()
		log.Printf("TCP listener stopped on port %d", port)
	}
	s.listeners = make(map[int]*portListener)
	s.mu.Unlock()

	s.wg.Wait()
}

func (s *Server) UpdateConfig(newConfig *config.Config) {
	oldRoutes := s.config.TCP.K8s.Routes
	newRoutes := newConfig.TCP.K8s.Routes
	oldSocat := s.config.TCP.K8s.Socat
	newSocat := newConfig.TCP.K8s.Socat

	// Stop listeners for removed route ports
	s.mu.Lock()
	for port := range oldRoutes {
		if _, exists := newRoutes[port]; !exists {
			if pl, ok := s.listeners[port]; ok {
				log.Printf("TCP route port %d removed, stopping listener", port)
				close(pl.stopChan)
				pl.listener.Close()
				delete(s.listeners, port)
			}
		}
	}

	// Stop listeners for removed socat ports
	for port := range oldSocat {
		if _, exists := newSocat[port]; !exists {
			if pl, ok := s.listeners[port]; ok {
				log.Printf("TCP socat port %d removed, stopping listener", port)
				close(pl.stopChan)
				pl.listener.Close()
				delete(s.listeners, port)
			}
		}
	}
	s.mu.Unlock()

	// Start listeners for new route ports
	for port := range newRoutes {
		if _, existed := oldRoutes[port]; !existed {
			log.Printf("TCP route port %d added, starting listener", port)
			if err := s.startListener(port, listenerTypeRoute); err != nil {
				log.Printf("Failed to start TCP listener on port %d: %v", port, err)
			}
		}
	}

	// Start listeners for new socat ports
	for port := range newSocat {
		if _, existed := oldSocat[port]; !existed {
			log.Printf("TCP socat port %d added, starting listener", port)
			if err := s.startListener(port, listenerTypeSocat); err != nil {
				log.Printf("Failed to start TCP socat listener on port %d: %v", port, err)
			}
		}
	}

	s.mu.Lock()
	s.config = newConfig
	s.verbose = newConfig.Verbose
	s.mu.Unlock()
}
