package tunnel

import "time"

func (t *Tunnel) LocalPort() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.localPort
}

func (t *Tunnel) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAccess = time.Now()
}

func (t *Tunnel) IdleDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Since(t.lastAccess)
}

func (t *Tunnel) Hostname() string {
	return t.hostname
}

func (t *Tunnel) Scheme() string {
	if t.config.Scheme == "" {
		return "http"
	}
	return t.config.Scheme
}

func (t *Tunnel) LastError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastError
}
