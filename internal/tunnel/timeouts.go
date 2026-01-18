package tunnel

import "time"

// Timeout constants for tunnel operations
const (
	// PortForwardReadyTimeout is the timeout for waiting for port forward to be ready
	PortForwardReadyTimeout = 30 * time.Second
)
