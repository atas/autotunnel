package httpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

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

	scheme := tunnel.Scheme()
	targetURL := &url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("127.0.0.1:%d", tunnel.LocalPort()),
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	if scheme == "https" {
		proxy.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = r.Host
		req.Header.Set("X-Forwarded-Proto", scheme)
		req.Header.Set("X-Forwarded-Host", r.Host)
		if r.RemoteAddr != "" {
			req.Header.Set("X-Forwarded-For", strings.Split(r.RemoteAddr, ":")[0])
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Don't log client disconnections - they're normal
		if err == context.Canceled || strings.Contains(err.Error(), "context canceled") {
			return
		}
		log.Printf("[http] [%s] Proxy error: %v", host, err)
		http.Error(w, fmt.Sprintf("Proxy error for host '%s': %v", host, err), http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}
