package httpserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/atas/autotunnel/internal/tunnelmgr"
)

// httpsBackendTransport is shared by all requests to scheme: https backends, so
// connections are pooled and closed when idle. It speaks HTTP/1.1 to the backend,
// because a custom TLS config turns off Transport's automatic HTTP/2.
var httpsBackendTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	IdleConnTimeout: HTTPSBackendIdleConnTimeout,
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if s.config.Verbose {
		log.Printf("[http] [%s] %s %s", host, r.Method, r.URL.Path)
	}

	tunnel, err := s.manager.GetOrCreateTunnel(host, "http")
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

	proxyStart := time.Now()
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
	// never saw the request, so get a fresh tunnel and send it again. A stall also
	// needs a fresh tunnel, but it can't be told apart from a slow backend dropping
	// the connection, so only send the request again if that is safe. Any other error
	// can come from the backend, so it leaves the shared tunnel alone.
	retry := isSafeToRepeat(r)
	if isDialError(proxyErr) {
		tunnel.MarkFailed(proxyErr)
		retry = true
	} else if s.isPortForwardStall(proxyErr, time.Since(proxyStart)) {
		tunnel.MarkFailed(proxyErr)
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

	if !tunnel.IsRunning() {
		if err := tunnel.Start(r.Context()); err != nil {
			log.Printf("[http] [%s] Retry failed to start tunnel: %v", host, err)
			http.Error(w, fmt.Sprintf("Failed to start tunnel: %v", err), http.StatusBadGateway)
			return
		}
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

// isPortForwardStall reports whether the connection was dropped the way a black-holed
// port-forward drops it. The SPDY connection stays open but silent, so the local
// listener still accepts, and client-go closes the connection when it times out
// waiting for a stream. It hasn't read the request by then, so the proxy usually sees
// a reset rather than EOF, and no error ever reaches the tunnel's state.
func (s *Server) isPortForwardStall(err error, elapsed time.Duration) bool {
	if elapsed < s.stallMin || elapsed > s.stallMax {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// isSafeToRepeat reports whether the request can be sent again after the backend
// may have received it: it has no side effects and no body already consumed.
func isSafeToRepeat(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.ContentLength == 0
	}
	return false
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

	if scheme == "https" {
		proxy.Transport = httpsBackendTransport
	}

	var proxyErr error
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		proxyErr = err
		// Don't write an HTTP error response here -- let the caller decide
		// whether to retry or report the error.
	}

	proxy.ServeHTTP(w, r)

	return proxyErr
}
