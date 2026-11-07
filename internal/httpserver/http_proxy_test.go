package httpserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/tunnelmgr"
)

// frontServer serves mgr's tunnels through a real http.Server, since request
// bodies and context cancellation behave differently there than with
// httptest.NewRecorder.
func frontServer(mgr Manager) *httptest.Server {
	return httptest.NewServer(NewServer(testHTTPConfig(), mgr))
}

func runningTunnel(port int) *retryMockTunnel {
	return &retryMockTunnel{running: true, localPort: port, scheme: "http"}
}

func listenerPort(l net.Listener) int {
	return l.Addr().(*net.TCPAddr).Port
}

// serverCapturingTunnel hands over the *http.Server serving the request, which
// net/http puts in the context passed to Start.
type serverCapturingTunnel struct {
	retryMockTunnel
	server chan *http.Server
}

func (c *serverCapturingTunnel) Start(ctx context.Context) error {
	c.server <- ctx.Value(http.ServerContextKey).(*http.Server)
	return fmt.Errorf("not needed")
}

// Whole-request deadlines cut streams, slow responses and slow uploads, so only
// the header read may be limited. Their real effect takes over 30s to show.
func TestServer_LimitsOnlyHeaderRead(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	tun := &serverCapturingTunnel{server: make(chan *http.Server, 1)}
	cfg := testHTTPConfig()
	cfg.HTTP.ListenAddr = addr
	server := NewServer(cfg, &retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})
	go func() { _ = server.Start() }()

	var resp *http.Response
	for i := 0; i < 100; i++ {
		if resp, err = http.Get("http://" + addr + "/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	defer func() { _ = server.Shutdown(context.Background()) }()

	s := <-tun.server
	if s.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want none", s.ReadTimeout)
	}
	if s.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want none", s.WriteTimeout)
	}
	if s.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want a limit", s.ReadHeaderTimeout)
	}
	if s.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want a limit", s.IdleTimeout)
	}
}

func TestServeHTTP_ClientAbortedUploadKeepsTunnel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer backend.Close()

	// The error depends on timing, so try more than once.
	for i := 0; i < 20; i++ {
		tun := runningTunnel(listenerPort(backend.Listener))
		mgr := &retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}}
		front := frontServer(mgr)

		conn, err := net.Dial("tcp", front.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprint(conn, "POST /upload HTTP/1.1\r\nHost: test.localhost\r\nContent-Length: 100000\r\n\r\nabc")
		time.Sleep(5 * time.Millisecond)
		_ = conn.Close()

		front.Close() // waits for the handler to finish

		if tun.markedFailed {
			t.Fatalf("attempt %d: client abort marked the tunnel failed: %v", i, tun.failedErr)
		}
		if len(mgr.getCalls) != 1 {
			t.Fatalf("attempt %d: got %d GetOrCreateTunnel calls, want 1", i, len(mgr.getCalls))
		}
	}
}

// A backend dropping a GET does not show that the tunnel is dead, and the retry
// goes through the same tunnel.
func TestServeHTTP_BackendAbortKeepsTunnelAndRetriesGET(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write([]byte("second try"))
	}))
	defer backend.Close()

	tun := runningTunnel(listenerPort(backend.Listener))
	front := frontServer(&retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	front.Close()

	if resp.StatusCode != http.StatusOK || string(body) != "second try" {
		t.Errorf("got %d %q, want 200 from the retry", resp.StatusCode, body)
	}
	if hits.Load() != 2 {
		t.Errorf("backend hits = %d, want 2", hits.Load())
	}
	if tun.markedFailed {
		t.Errorf("backend abort marked the tunnel failed: %v", tun.failedErr)
	}
}

// The backend reads the whole request and then drops the connection, so it may
// already have acted on it. Only requests that are safe to repeat may be retried.
func TestServeHTTP_RetryOnlyWhenSafe(t *testing.T) {
	tests := []struct {
		method   string
		body     string
		wantHits int32
	}{
		{"GET", "", 2},
		{"HEAD", "", 2},
		{"OPTIONS", "", 2},
		{"GET", "payload", 1},
		{"POST", "", 1},
		{"POST", "payload", 1},
		{"PUT", "payload", 1},
		{"PATCH", "payload", 1},
		{"DELETE", "", 1},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s body=%q", tt.method, tt.body), func(t *testing.T) {
			var hits atomic.Int32
			var firstBody atomic.Value
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if hits.Add(1) == 1 {
					firstBody.Store(string(body))
				}
				panic(http.ErrAbortHandler)
			}))
			defer backend.Close()

			tun := runningTunnel(listenerPort(backend.Listener))
			front := frontServer(&retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})

			req, err := http.NewRequest(tt.method, front.URL+"/charge?id=5", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			front.Close()

			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", resp.StatusCode)
			}
			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("backend hits = %d, want %d", got, tt.wantHits)
			}
			if got := firstBody.Load(); got != tt.body {
				t.Errorf("backend got body %q, want %q", got, tt.body)
			}
			if tun.markedFailed {
				t.Errorf("tunnel marked failed: %v", tun.failedErr)
			}
		})
	}
}

// A failed dial means the backend never saw the request, so it is safe to retry
// any method, and the body must still be complete. It is larger than what
// net/http drains when a request body is closed.
func TestServeHTTP_DialFailureRetriesPOSTWithBody(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := listenerPort(dead)
	_ = dead.Close()

	var hits atomic.Int32
	var received atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		received.Store(string(body))
	}))
	defer backend.Close()

	deadTun := runningTunnel(deadPort)
	freshTun := runningTunnel(listenerPort(backend.Listener))
	front := frontServer(&retryMockManager{tunnels: []tunnelmgr.TunnelHandle{deadTun, freshTun}})

	payload := strings.Repeat("x", 300*1024)
	resp, err := http.Post(front.URL+"/upload", "text/plain", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	front.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !deadTun.markedFailed {
		t.Error("dead tunnel was not marked failed")
	}
	if freshTun.markedFailed {
		t.Error("fresh tunnel was marked failed")
	}
	if hits.Load() != 1 {
		t.Errorf("backend hits = %d, want 1", hits.Load())
	}
	if got, _ := received.Load().(string); got != payload {
		t.Errorf("backend got %d body bytes, want %d", len(got), len(payload))
	}
}

// droppingBackend accepts a connection and closes it after delay, the way a backend
// that times out a slow request does, or a port-forward whose stream could not be
// opened. With drain, it reads the request first, so the proxy sees EOF, not a reset.
func droppingBackend(t *testing.T, delay time.Duration, drain bool) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			if drain {
				go func() { _, _ = io.Copy(io.Discard, conn) }()
			}
			time.AfterFunc(delay, func() { _ = conn.Close() })
		}
	}()
	return listenerPort(l)
}

// A dropped connection can't be told apart from the backend timing the request out,
// so it never fails the shared tunnel and its other streams. The request is sent
// again only if that is safe. Like the real manager, the mock hands out the same
// tunnel again, as it is not failed, so the retry is dropped too. (A black-holed
// port-forward fails itself when it can't open a stream, and the retry then gets a
// fresh tunnel.)
func TestServeHTTP_DroppedConnectionKeepsTunnel(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		body         string
		drain        bool
		wantStatus   int
		wantGetCalls int
	}{
		{"GET retried", "GET", "", false, http.StatusBadGateway, 2},
		{"GET dropped with EOF", "GET", "", true, http.StatusBadGateway, 2},
		{"POST not retried", "POST", "payload", false, http.StatusBadGateway, 1},
		{"POST larger than socket buffers", "POST", strings.Repeat("x", 8<<20), false, http.StatusBadGateway, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			droppingTun := runningTunnel(droppingBackend(t, 100*time.Millisecond, tt.drain))
			mgr := &retryMockManager{tunnels: []tunnelmgr.TunnelHandle{droppingTun}}
			front := frontServer(mgr)
			defer front.Close()

			req, err := http.NewRequest(tt.method, front.URL+"/", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if droppingTun.markedFailed {
				t.Errorf("tunnel marked failed: %v", droppingTun.failedErr)
			}
			if len(mgr.getCalls) != tt.wantGetCalls {
				t.Errorf("GetOrCreateTunnel calls = %d, want %d", len(mgr.getCalls), tt.wantGetCalls)
			}
		})
	}
}

// A request body that turns out to be invalid is the client's fault, not the
// port-forward dropping the connection.
func TestServeHTTP_BadRequestBodyKeepsTunnel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer backend.Close()

	tun := runningTunnel(listenerPort(backend.Listener))
	front := frontServer(&retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: test.localhost\r\nTransfer-Encoding: chunked\r\n\r\nnot-a-chunk-size\r\n")

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	_ = resp.Body.Close()
	_ = conn.Close()
	front.Close()

	if tun.markedFailed {
		t.Errorf("tunnel marked failed: %v", tun.failedErr)
	}
}

// A client that half-closes is still reading, but net/http cancels the request
// context. It must not be told the request succeeded.
func TestServeHTTP_CancelledRequestIsNotReportedAs200(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	}))
	defer backend.Close()

	tun := runningTunnel(listenerPort(backend.Listener))
	front := frontServer(&retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(conn, "GET /slow HTTP/1.1\r\nHost: test.localhost\r\n\r\n")

	for hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	_ = resp.Body.Close()
	_ = conn.Close()
	front.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("backend hits = %d, want 1", hits.Load())
	}
	if tun.markedFailed {
		t.Errorf("tunnel marked failed: %v", tun.failedErr)
	}
}

func TestServeHTTP_HTTPSBackendReusesOneConnection(t *testing.T) {
	var opened, http2 atomic.Int32
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			http2.Add(1)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	backend.EnableHTTP2 = true
	backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	backend.StartTLS()
	defer backend.Close()

	tun := runningTunnel(listenerPort(backend.Listener))
	tun.scheme = "https"
	server := NewServer(testHTTPConfig(), &retryMockManager{tunnels: []tunnelmgr.TunnelHandle{tun}})

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "test.localhost"
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}

	if got := opened.Load(); got != 1 {
		t.Errorf("backend connections opened = %d, want 1", got)
	}
	if got := http2.Load(); got != 0 {
		t.Errorf("%d requests reached the backend over HTTP/2, want HTTP/1.1", got)
	}
}

func TestIsSafeToRepeat(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		body    string
		upgrade string
		want    bool
	}{
		{"GET", "GET", "", "", true},
		{"HEAD", "HEAD", "", "", true},
		{"GET with body", "GET", "x", "", false},
		{"POST", "POST", "", "", false},
		{"WebSocket upgrade", "GET", "", "websocket", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", strings.NewReader(tt.body))
			if tt.body == "" {
				r.ContentLength = 0
			}
			if tt.upgrade != "" {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", tt.upgrade)
			}
			if got := isSafeToRepeat(r); got != tt.want {
				t.Errorf("isSafeToRepeat = %v, want %v", got, tt.want)
			}
		})
	}
}
