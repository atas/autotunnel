package httpserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/atas/autotunnel/internal/tunnelmgr"
)

// httpsBackendTransport is shared by all requests to scheme: https backends, so
// connections are pooled and closed when idle. It speaks HTTP/1.1 to the backend,
// because a custom TLS config turns off Transport's automatic HTTP/2.
var httpsBackendTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	IdleConnTimeout: HTTPSBackendIdleConnTimeout,
}

// isDynamicHost reports whether host resolves through dynamic routing. Static
// routes take priority, as they do in the manager.
func (s *Server) isDynamicHost(host string) bool {
	if _, static := s.config.HTTP.K8s.Routes[host]; static {
		return false
	}
	_, dynamic := tunnelmgr.ParseDynamicHostname(host, s.config.HTTP.K8s.DynamicHost, "http")
	return dynamic
}

// allowedOnDynamicHost refuses requests a browser made on behalf of another page,
// because a dynamic hostname lets that page choose the context, namespace and
// target. Links still work: a top-level GET or HEAD navigation is let through.
// Clients sending neither header (curl, scripts) pass, as they can set any header.
func allowedOnDynamicHost(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site": // same-site: every dynamic host shares one site
		navigation := r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
		if !navigation || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			return false
		}
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// "null", sent by sandboxed pages and cross-site form POSTs, parses with an empty Host
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// stripCookieDomains keeps a backend's cookies on its own dynamic host, so they
// cannot reach the other tunnels under dynamic_host. This only covers Set-Cookie
// headers seen here: a page can still set the same cookie from script, and TLS
// passthrough responses are never seen at all.
func stripCookieDomains(resp *http.Response) error {
	cookies := resp.Header["Set-Cookie"]
	for i, cookie := range cookies {
		// The first part is name=value, even for a cookie named "domain"
		parts := strings.Split(cookie, ";")
		kept := parts[:1]
		for _, attr := range parts[1:] {
			name, _, _ := strings.Cut(attr, "=")
			if !strings.EqualFold(strings.TrimSpace(name), "domain") {
				kept = append(kept, attr)
			}
		}
		cookies[i] = strings.Join(kept, ";")
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if s.config.Verbose {
		log.Printf("[http] [%s] %s %s", host, r.Method, r.URL.Path)
	}

	// WebSocket upgrades arrive here like any other request
	if s.isDynamicHost(host) && !allowedOnDynamicHost(r) {
		log.Printf("[http] [%s] Refused request from another page (Origin: %q, Sec-Fetch-Site: %q)",
			host, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "Dynamic routes refuse requests from other pages. See the Security Note in the autotunnel README.", http.StatusForbidden)
		return
	}

	tunnel, err := s.manager.GetOrCreateTunnel(host, "http")
	if err != nil {
		log.Printf("[http] [%s] Error: %v", host, err)
		http.Error(w, fmt.Sprintf("No service configured for host: %s", host), http.StatusBadGateway)
		return
	}

	if tunnel, err = s.startTunnel(r.Context(), host, tunnel); err != nil {
		log.Printf("[http] [%s] Failed to start tunnel: %v", host, err)
		http.Error(w, fmt.Sprintf("Failed to start tunnel: %v", err), http.StatusBadGateway)
		return
	}

	tunnel.Touch()

	proxyErr := s.proxyRequest(w, r, host, tunnel)
	if proxyErr == nil {
		return
	}

	// The client's connection ended, which says nothing about the tunnel. It may
	// still be reading (a half-close also cancels the context), so answer it:
	// returning without a response would make net/http send a 200.
	if r.Context().Err() != nil {
		http.Error(w, "Request cancelled", http.StatusBadGateway)
		return
	}

	log.Printf("[http] [%s] Proxy error: %v", host, proxyErr)

	// A failed dial means the port-forward's local listener is gone and the backend
	// never saw the request, so get a fresh tunnel and send it again. Any other error
	// can come from the backend, so it leaves the shared tunnel alone, and the request
	// is only sent again if that is safe. A black-holed port-forward is caught by the
	// tunnel itself, when it fails to open a stream, so the retry gets a fresh one.
	retry := isSafeToRepeat(r)
	if isDialError(proxyErr) {
		tunnel.MarkFailed(proxyErr)
		retry = true
	}
	if !retry {
		http.Error(w, fmt.Sprintf("Proxy error for host '%s': %v", host, proxyErr), http.StatusBadGateway)
		return
	}
	log.Printf("[http] [%s] Retrying", host)

	tunnel, err = s.manager.GetOrCreateTunnel(host, "http")
	if err != nil {
		log.Printf("[http] [%s] Retry error: %v", host, err)
		http.Error(w, fmt.Sprintf("Proxy error for host '%s': %v", host, proxyErr), http.StatusBadGateway)
		return
	}

	if tunnel, err = s.startTunnel(r.Context(), host, tunnel); err != nil {
		log.Printf("[http] [%s] Retry failed to start tunnel: %v", host, err)
		http.Error(w, fmt.Sprintf("Failed to start tunnel: %v", err), http.StatusBadGateway)
		return
	}

	tunnel.Touch()

	if retryErr := s.proxyRequest(w, r, host, tunnel); retryErr != nil {
		// Retry also failed, send error to client
		log.Printf("[http] [%s] Retry proxy error: %v", host, retryErr)
		http.Error(w, fmt.Sprintf("Proxy error for host '%s': %v", host, retryErr), http.StatusBadGateway)
	}
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// isSafeToRepeat reports whether the request can be sent again after the backend
// may have received it: it has no side effects and no body already consumed.
func isSafeToRepeat(r *http.Request) bool {
	// An upgrade can fail after the proxy has taken over the client's connection
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.ContentLength == 0
	}
	return false
}

// startTunnel starts tun unless it is running. If another request saw it fail after the
// manager handed it out, the manager has a replacement, which is started instead.
func (s *Server) startTunnel(ctx context.Context, host string, tun tunnelmgr.TunnelHandle) (tunnelmgr.TunnelHandle, error) {
	if tun.IsRunning() {
		return tun, nil
	}
	err := tun.Start(ctx)
	if errors.Is(err, tunnelmgr.ErrTunnelFailed) {
		if tun, err = s.manager.GetOrCreateTunnel(host, "http"); err == nil {
			err = tun.Start(ctx)
		}
	}
	return tun, err
}

// proxyRequest forwards the HTTP request to the tunnel's local port.
// Returns a non-nil error if the proxy got no response from the backend, in which
// case nothing was written to the client and the caller must respond.
// Returns nil when the response was proxied.
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, host string, tun tunnelmgr.TunnelHandle) error {
	scheme := tun.Scheme()
	targetURL := &url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("127.0.0.1:%d", tun.LocalPort()),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Host = pr.In.Host
			// Rewrite strips query parameters Go can't parse, such as ones using
			// ";". Keep the original string so the backend sees what the client sent.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", scheme)
		},
	}

	if s.isDynamicHost(host) {
		proxy.ModifyResponse = stripCookieDomains
	}

	if scheme == "https" {
		proxy.Transport = httpsBackendTransport
	}

	var proxyErr error
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		proxyErr = err
		// Don't write an HTTP error response here -- let the caller decide
		// whether to retry or report the error.
	}

	tun.Acquire()
	defer tun.Release()
	proxy.ServeHTTP(w, r)

	return proxyErr
}
