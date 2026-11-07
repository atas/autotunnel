package httpserver

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/atas/autotunnel/internal/config"
)

const (
	testDynamicHost = "nginx-80.svc.default.ns.microk8s.cx.k8s.localhost"
	// A static route whose name also matches the dynamic pattern
	testPinnedHost = "pinned-80.svc.default.ns.microk8s.cx.k8s.localhost"
)

// newDynamicTestServer returns a server with dynamic routing on, two static
// routes, and a backend behind a mock tunnel.
func newDynamicTestServer(t *testing.T, backend http.HandlerFunc) (*Server, *mockManager) {
	t.Helper()
	ts := httptest.NewServer(backend)
	t.Cleanup(ts.Close)

	cfg := testHTTPConfig()
	cfg.HTTP.K8s.DynamicHost = "k8s.localhost"
	cfg.HTTP.K8s.Routes = map[string]config.K8sRouteConfig{
		"static.localhost": {Context: "c", Namespace: "n", Service: "s", Port: 80},
		testPinnedHost:     {Context: "c", Namespace: "n", Service: "s", Port: 80},
	}
	mgr := &mockManager{tunnel: &mockTunnel{
		running:   true,
		localPort: ts.Listener.Addr().(*net.TCPAddr).Port,
	}}
	return NewServer(cfg, mgr), mgr
}

func TestServer_ServeHTTP_DynamicHostRequestCheck(t *testing.T) {
	// The foreign header sets follow what Chrome sent in real runs against autotunnel.
	crossSiteFormPost := map[string]string{
		"Origin": "null", "Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
	}

	tests := []struct {
		name    string
		method  string
		host    string // default: testDynamicHost on port 8989
		headers map[string]string
		want    int
	}{
		{
			name:    "cross-site form POST with Origin null",
			method:  "POST",
			headers: crossSiteFormPost,
			want:    http.StatusForbidden,
		},
		{
			name:   "same-site no-cors POST from another dynamic host",
			method: "POST",
			headers: map[string]string{
				"Origin":         "http://evil-80.svc.dev.ns.dev.cx.k8s.localhost:8989",
				"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "empty",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "same-site POST navigation without Origin",
			method: "POST",
			headers: map[string]string{
				"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "cross-site iframe",
			method: "GET",
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "cross-site img",
			method: "GET",
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "cross-site WebSocket upgrade",
			method: "GET",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket",
				"Origin":         "http://attacker.localhost:19000",
				"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "websocket", "Sec-Fetch-Dest": "websocket",
			},
			want: http.StatusForbidden,
		},
		{
			name:   "WebSocket upgrade with foreign Origin and no Sec-Fetch",
			method: "GET",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket", "Origin": "http://attacker.localhost:19000",
			},
			want: http.StatusForbidden,
		},
		{
			name:    "POST with Origin null and no Sec-Fetch",
			method:  "POST",
			headers: map[string]string{"Origin": "null"},
			want:    http.StatusForbidden,
		},
		{
			name:    "Origin differs only by port",
			method:  "POST",
			headers: map[string]string{"Origin": "http://" + testDynamicHost + ":9000"},
			want:    http.StatusForbidden,
		},
		{
			name:    "malformed Origin",
			method:  "POST",
			headers: map[string]string{"Origin": "not an origin"},
			want:    http.StatusForbidden,
		},

		{
			name:   "cross-site GET top-level navigation",
			method: "GET",
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
			},
			want: http.StatusOK,
		},
		{
			name:   "same-site HEAD top-level navigation",
			method: "HEAD",
			headers: map[string]string{
				"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
			},
			want: http.StatusOK,
		},
		{
			name:   "typed into the address bar",
			method: "GET",
			headers: map[string]string{
				"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
			},
			want: http.StatusOK,
		},
		{
			name:   "same-origin POST",
			method: "POST",
			headers: map[string]string{
				"Origin":         "http://" + testDynamicHost + ":8989",
				"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
			},
			want: http.StatusOK,
		},
		{name: "curl GET", method: "GET", want: http.StatusOK},
		{name: "script POST", method: "POST", want: http.StatusOK},

		{
			name:    "static route accepts cross-site POST",
			method:  "POST",
			host:    "static.localhost:8989",
			headers: crossSiteFormPost,
			want:    http.StatusOK,
		},
		{
			name:    "static route wins over a matching dynamic pattern",
			method:  "POST",
			host:    testPinnedHost + ":8989",
			headers: crossSiteFormPost,
			want:    http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var backendHit atomic.Bool
			server, mgr := newDynamicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				backendHit.Store(true)
			})

			req := httptest.NewRequest(tt.method, "/admin", nil)
			req.Host = tt.host
			if req.Host == "" {
				req.Host = testDynamicHost + ":8989"
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
			if tt.want == http.StatusForbidden && (len(mgr.getCalls) != 0 || backendHit.Load()) {
				t.Errorf("refused request reached manager %v, backend %v", mgr.getCalls, backendHit.Load())
			}
			if tt.want == http.StatusOK && !backendHit.Load() {
				t.Error("allowed request did not reach the backend")
			}
		})
	}
}

func TestServer_ServeHTTP_SetCookieDomain(t *testing.T) {
	cookies := []string{
		"tossed=fromevil; Domain=k8s.localhost",
		"session=abc; Path=/; domain=.k8s.localhost; HttpOnly; SameSite=Lax",
		"domain=keepme; Path=/",
		"plain=1",
	}
	stripped := []string{
		"tossed=fromevil",
		"session=abc; Path=/; HttpOnly; SameSite=Lax",
		"domain=keepme; Path=/",
		"plain=1",
	}

	tests := []struct {
		name string
		host string
		want []string
	}{
		{"dynamic host", testDynamicHost + ":8989", stripped},
		{"static host", "static.localhost:8989", cookies},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := newDynamicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				for _, c := range cookies {
					w.Header().Add("Set-Cookie", c)
				}
			})

			req := httptest.NewRequest("GET", "/", nil)
			req.Host = tt.host
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			got := w.Result().Header.Values("Set-Cookie")
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("Set-Cookie = %q, want %q", got, tt.want)
			}
		})
	}
}

// A same-origin WebSocket must keep working, and its handshake response gets the
// same cookie treatment as any other response.
func TestServer_ServeHTTP_DynamicWebSocketSameOrigin(t *testing.T) {
	server, _ := newDynamicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack failed: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Set-Cookie: tossed=1; Domain=k8s.localhost\r\n\r\n")
		_ = buf.Flush()
	})
	front := httptest.NewServer(server)
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/ws", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Host = testDynamicHost + ":8989"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "http://"+testDynamicHost+":8989")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "websocket")
	req.Header.Set("Sec-Fetch-Dest", "websocket")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Set-Cookie"); got != "tossed=1" {
		t.Errorf("Set-Cookie = %q, want the Domain attribute stripped", got)
	}
}
