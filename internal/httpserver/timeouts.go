package httpserver

import "time"

// Timeout constants for HTTP server operations
const (
	// TLSErrorPageDeadline is the deadline for completing TLS error page handshake
	TLSErrorPageDeadline = 10 * time.Second

	// TLSClientHelloDeadline is the deadline for reading TLS ClientHello
	TLSClientHelloDeadline = 10 * time.Second

	// TLSTunnelStartTimeout is the timeout for starting a tunnel for TLS connections
	TLSTunnelStartTimeout = 30 * time.Second

	// TLSBackendDialTimeout is the timeout for dialing the backend service
	TLSBackendDialTimeout = 10 * time.Second
)
