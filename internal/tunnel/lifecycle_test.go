package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"      //nolint:staticcheck // client-go's portforward still takes these types
	"k8s.io/apimachinery/pkg/util/httpstream/spdy" //nolint:staticcheck // client-go's portforward still takes these types
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// fakeAPIServer speaks just enough SPDY port-forward for the real Tunnel and
// client-go code to run. Data streams echo unless quietBackend is set.
type fakeAPIServer struct {
	srv        *httptest.Server
	restConfig *rest.Config
	clientset  kubernetes.Interface

	dialDelay     atomic.Int64 // nanoseconds before each SPDY upgrade
	rejectStreams atomic.Bool  // reset new streams, as a dying connection does
	quietBackend  atomic.Bool  // accept connections, never send a byte
	dials         atomic.Int64

	mu    sync.Mutex
	conns map[httpstream.Connection]struct{}
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{conns: make(map[httpstream.Connection]struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.portForward))
	t.Cleanup(func() {
		f.killConnections()
		f.srv.Close()
	})
	f.restConfig = &rest.Config{Host: f.srv.URL}
	clientset, err := kubernetes.NewForConfig(f.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	f.clientset = clientset
	return f
}

func (f *fakeAPIServer) portForward(w http.ResponseWriter, r *http.Request) {
	time.Sleep(time.Duration(f.dialDelay.Load()))

	if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
		return
	}
	// Counted before the upgrade: the client can go on as soon as it has the response
	f.dials.Add(1)
	streams := make(chan httpstream.Stream, 64)
	conn := spdy.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
		if f.rejectStreams.Load() {
			return errors.New("rejected")
		}
		streams <- s
		return nil
	})
	if conn == nil {
		return
	}
	f.mu.Lock()
	f.conns[conn] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, conn)
		f.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		select {
		case <-conn.CloseChan():
			return
		case s := <-streams:
			if s.Headers().Get(corev1.StreamType) == corev1.StreamTypeError {
				s.Close() // closing it empty means "no error" to client-go
				continue
			}
			go f.serveData(s)
		}
	}
}

func (f *fakeAPIServer) serveData(data httpstream.Stream) {
	defer data.Close()
	if f.quietBackend.Load() {
		_, _ = io.Copy(io.Discard, data)
		return
	}
	_, _ = io.Copy(data, data)
}

// killConnections closes every port-forward from the server side, like a pod restart.
func (f *fakeAPIServer) killConnections() {
	f.mu.Lock()
	conns := make([]httpstream.Connection, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeAPIServer) activeConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func (f *fakeAPIServer) newTunnel() *Tunnel {
	cfg := config.K8sRouteConfig{Namespace: "ns", Pod: "pod", Port: 80}
	return NewTunnel("test.localhost", cfg, f.clientset, f.restConfig, "", false)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *Tunnel) hasStopChan() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.stopChan != nil
}

func dialTunnel(t *testing.T, tun *Tunnel) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tun.LocalPort())), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestFakeAPIServer_ForwardsTraffic(t *testing.T) {
	f := newFakeAPIServer(t)
	tun := f.newTunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()

	conn := dialTunnel(t, tun)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("got %q", buf)
	}
}

// A handler holding a stale handle must not bring a failed tunnel back, and a
// second MarkFailed on it must not close the stop channel again.
func TestTunnel_Start_RefusesFailedTunnel(t *testing.T) {
	f := newFakeAPIServer(t)
	tun := f.newTunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	tun.MarkFailed(errors.New("backend dial failed"))

	if err := tun.Start(context.Background()); err == nil {
		t.Error("Start on a failed tunnel should return an error")
	}
	if got := tun.State(); got != StateFailed {
		t.Errorf("state = %v, want %v", got, StateFailed)
	}
	if got := f.dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1: a failed tunnel must not open a new port-forward", got)
	}

	tun.MarkFailed(errors.New("second failure")) // must not panic
}

// Used to panic with "close of closed channel".
func TestTunnel_MarkFailedWhileStarting_ThenCancel(t *testing.T) {
	f := newFakeAPIServer(t)
	f.dialDelay.Store(int64(300 * time.Millisecond))
	tun := f.newTunnel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tun.Start(ctx) }()

	waitFor(t, "the port-forward to be created", tun.hasStopChan)
	tun.MarkFailed(errors.New("backend dial failed"))
	cancel()

	if err := <-done; err == nil {
		t.Error("Start should return an error")
	}
	if got := tun.State(); got != StateFailed {
		t.Errorf("state = %v, want %v: cancelling must not hide the failure", got, StateFailed)
	}
}

// Used to overwrite Failed with Running and return nil.
func TestTunnel_MarkFailedWhileStarting_StaysFailed(t *testing.T) {
	f := newFakeAPIServer(t)
	f.dialDelay.Store(int64(200 * time.Millisecond))
	tun := f.newTunnel()

	done := make(chan error, 1)
	go func() { done <- tun.Start(context.Background()) }()

	waitFor(t, "the port-forward to be created", tun.hasStopChan)
	tun.MarkFailed(errors.New("backend dial failed"))

	if err := <-done; err == nil {
		t.Error("Start should return an error when the tunnel failed while starting")
	}
	if got := tun.State(); got != StateFailed {
		t.Errorf("state = %v, want %v", got, StateFailed)
	}
	waitFor(t, "the port-forward to close", func() bool { return f.activeConnections() == 0 })
}

// MarkFailed can land while the pod is still being looked up, before there is a stop channel.
func TestTunnel_MarkFailedBeforePortForwardCreated(t *testing.T) {
	f := newFakeAPIServer(t)
	tun := f.newTunnel()
	tun.state = StateStarting

	tun.MarkFailed(errors.New("backend dial failed"))

	if err := tun.startPortForward(context.Background()); err == nil {
		t.Error("startPortForward should return an error")
	}
	if got := tun.State(); got != StateFailed {
		t.Errorf("state = %v, want %v", got, StateFailed)
	}
	// Give an unwanted dial time to show up.
	time.Sleep(100 * time.Millisecond)
	if got := f.dials.Load(); got != 0 {
		t.Errorf("dials = %d, want 0", got)
	}
}

// Runs handler-shaped goroutines against a port-forward that keeps dying, with a
// manager-shaped replacement of failed tunnels. Meant for go test -race: both the
// double close and the stopChan data race show up here. It is paced, because each
// connection uses an ephemeral port and an unpaced run can exhaust them.
func TestTunnel_BurstDuringPortForwardDeath(t *testing.T) {
	f := newFakeAPIServer(t)

	var mu sync.Mutex
	current := f.newTunnel()
	all := []*Tunnel{current}
	getOrCreate := func() *Tunnel {
		mu.Lock()
		defer mu.Unlock()
		if current.State() == StateFailed {
			current = f.newTunnel()
			all = append(all, current)
		}
		return current
	}

	deadline := time.Now().Add(time.Second)
	var wg sync.WaitGroup

	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				tun := getOrCreate()
				if !tun.IsRunning() {
					// Some callers give up mid-start, like a cancelled HTTP request.
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i+1)*time.Millisecond)
					err := tun.Start(ctx)
					cancel()
					if err != nil {
						continue
					}
				}
				conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tun.LocalPort())), time.Second)
				if err != nil {
					tun.MarkFailed(err)
					continue
				}
				_ = conn.(*net.TCPConn).SetLinger(0) // no TIME_WAIT
				conn.Close()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			f.killConnections()
			time.Sleep(10 * time.Millisecond)
			getOrCreate().Stop() // what idle cleanup does
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Wait()

	if len(all) < 2 {
		t.Errorf("only %d tunnel created, the port-forward never failed during the test", len(all))
	}

	// A stop channel still set would be a port-forward nobody can stop any more
	for _, tun := range all {
		tun.Stop()
		if tun.hasStopChan() {
			t.Errorf("tunnel in state %v still has a port-forward running", tun.State())
		}
	}
}

// TUN-7: a stream the API server will not open is what a dead port-forward looks
// like. The next connection must get a fresh tunnel.
func TestTunnel_StreamFailureMarksTunnelFailed(t *testing.T) {
	f := newFakeAPIServer(t)
	tun := f.newTunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()

	f.rejectStreams.Store(true)
	dialTunnel(t, tun)

	waitFor(t, "the tunnel to be marked failed", func() bool { return tun.State() == StateFailed })
	if tun.LastError() == nil {
		t.Error("LastError should say why the tunnel failed")
	}
}

// TUN-7: a backend that says nothing is not a dead port-forward. Silent servers,
// client-first protocols and connections closed without a byte must leave it up.
func TestTunnel_QuietBackendKeepsTunnelRunning(t *testing.T) {
	f := newFakeAPIServer(t)
	f.quietBackend.Store(true)
	tun := f.newTunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()

	conn := dialTunnel(t, tun)
	_, _ = conn.Write([]byte("hello?"))
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("expected a silent backend, got n=%d err=%v", n, err)
	}
	conn.Close()
	time.Sleep(100 * time.Millisecond)

	if got := tun.State(); got != StateRunning {
		t.Errorf("state = %v, want %v after a connection with no backend bytes", got, StateRunning)
	}

	f.quietBackend.Store(false)
	echo := dialTunnel(t, tun)
	if _, err := echo.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = echo.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(echo, make([]byte, 1)); err != nil {
		t.Errorf("tunnel stopped forwarding after a quiet connection: %v", err)
	}
}

func TestTunnel_AcquireRelease(t *testing.T) {
	tun := &Tunnel{lastAccess: time.Now().Add(-time.Hour)}

	tun.Acquire()
	tun.Acquire()
	if got := tun.ActiveConns(); got != 2 {
		t.Errorf("ActiveConns = %d, want 2", got)
	}
	if tun.IdleDuration() < time.Hour {
		t.Error("Acquire should not refresh lastAccess")
	}

	tun.Release()
	if tun.IdleDuration() > time.Minute {
		t.Error("Release should refresh lastAccess, so the timeout counts from the end of the connection")
	}

	tun.Release()
	if got := tun.ActiveConns(); got != 0 {
		t.Errorf("ActiveConns = %d, want 0", got)
	}
}

// A late error from a port-forward that was already stopped must not fail its replacement.
func TestTunnel_LateErrorFromOldPortForwardIsIgnored(t *testing.T) {
	old := make(chan struct{})
	current := make(chan struct{})
	tun := &Tunnel{hostname: "test.localhost", state: StateRunning, stopChan: current}

	errChan := make(chan error, 1)
	errChan <- errors.New("lost connection to pod")
	tun.monitorErrors(errChan, old)

	if got := tun.State(); got != StateRunning {
		t.Errorf("state = %v, want %v", got, StateRunning)
	}

	errChan <- errors.New("lost connection to pod")
	tun.monitorErrors(errChan, current)

	if got := tun.State(); got != StateFailed {
		t.Errorf("state = %v, want %v", got, StateFailed)
	}
}
