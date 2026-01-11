package tunnel

import "time"

// LocalPort returns the local port the tunnel is listening on
func (t *Tunnel) LocalPort() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.localPort
}

// Touch updates the last access time
func (t *Tunnel) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAccess = time.Now()
}

// IdleDuration returns how long the tunnel has been idle
func (t *Tunnel) IdleDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Since(t.lastAccess)
}

// Hostname returns the hostname this tunnel serves
func (t *Tunnel) Hostname() string {
	return t.hostname
}

// Scheme returns the scheme (http/https) for X-Forwarded-Proto header
func (t *Tunnel) Scheme() string {
	if t.config.Scheme == "" {
		return "http"
	}
	return t.config.Scheme
}

// LastError returns the last error encountered by the tunnel
func (t *Tunnel) LastError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastError
}
