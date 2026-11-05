package httpserver

import "time"

// Timeout constants for HTTP server operations
const (
	// HTTPReadHeaderTimeout is the deadline for a client to send its request headers
	HTTPReadHeaderTimeout = 30 * time.Second

	// HTTPSBackendIdleConnTimeout is how long an unused connection to a scheme: https
	// backend stays open. Each one holds a port-forward stream.
	HTTPSBackendIdleConnTimeout = 90 * time.Second

	// PortForwardStallMin and PortForwardStallMax bracket the 30s client-go waits to
	// open a stream on a port-forward connection before dropping the local connection.
	PortForwardStallMin = 25 * time.Second
	PortForwardStallMax = 35 * time.Second

	// TLSErrorPageDeadline is the deadline for completing TLS error page handshake
	TLSErrorPageDeadline = 10 * time.Second

	// TLSClientHelloDeadline is the deadline for reading TLS ClientHello
	TLSClientHelloDeadline = 10 * time.Second

	// TLSTunnelStartTimeout is the timeout for starting a tunnel for TLS connections
	TLSTunnelStartTimeout = 30 * time.Second

	// TLSBackendDialTimeout is the timeout for dialing the backend service
	TLSBackendDialTimeout = 10 * time.Second
)
