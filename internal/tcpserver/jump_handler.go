package tcpserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/k8sutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// jumpCloseGrace is how long a jump connection stays open without traffic after
// one direction has ended. A client can half-close and still wait for a response,
// and that cannot be told apart from a full close, so this is the only limit on
// that wait. socat gets the same value as its -t.
const jumpCloseGrace = 30 * time.Second

// jumpPodPollInterval is how often a pod being deleted is checked for removal.
const jumpPodPollInterval = 1 * time.Second

// JumpHandler forwards TCP via kubectl exec + socat into a pod.
// Used for reaching VPC-internal services like RDS through a jump pod.
type JumpHandler struct {
	route      config.JumpRouteConfig
	kubeconfig []string
	clientset  kubernetes.Interface
	restConfig *rest.Config
	verbose    bool
}

func NewJumpHandler(route config.JumpRouteConfig, kubeconfig []string, clientset kubernetes.Interface, restConfig *rest.Config, verbose bool) *JumpHandler {
	return &JumpHandler{
		route:      route,
		kubeconfig: kubeconfig,
		clientset:  clientset,
		restConfig: restConfig,
		verbose:    verbose,
	}
}

// HandleConnection forwards a TCP connection through the jump pod using exec + socat/nc
func (h *JumpHandler) HandleConnection(ctx context.Context, conn net.Conn, localPort int) error {
	// Check for required restConfig
	if h.restConfig == nil {
		return fmt.Errorf("restConfig is nil, cannot create SPDY executor")
	}

	// Discover the jump pod
	podName, containerName, err := h.discoverJumpPod(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover jump pod: %w", err)
	}

	if h.verbose {
		if h.route.Via.Service != "" {
			log.Printf("[jump:%d] Connecting via service %s (pod %s/%s) to %s:%d",
				localPort, h.route.Via.Service, h.route.Namespace, podName, h.route.Target.Host, h.route.Target.Port)
		} else {
			log.Printf("[jump:%d] Connecting via pod %s/%s to %s:%d",
				localPort, h.route.Namespace, podName, h.route.Target.Host, h.route.Target.Port)
		}
	}

	cmd, err := h.buildForwardCommand()
	if err != nil {
		return fmt.Errorf("failed to build forward command: %w", err)
	}

	req := h.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(h.route.Namespace).
		Name(podName).
		SubResource("exec")

	execOpts := &corev1.PodExecOptions{
		Command: []string{"sh", "-c", cmd},
		Stdin:   true,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}
	if containerName != "" {
		execOpts.Container = containerName
	}

	req.VersionedParams(execOpts, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(h.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	stderrReader, stderrWriter := io.Pipe()
	defer stderrWriter.Close() // ensures cleanup even on panic

	go func() {
		defer stderrReader.Close()
		buf := make([]byte, 4096)
		for {
			n, err := stderrReader.Read(buf)
			if n > 0 {
				stderrMsg := strings.TrimSpace(string(buf[:n]))
				// Log connection errors non-verbose (these are important)
				if isConnectionError(stderrMsg) {
					log.Printf("[jump:%d] Connection error: %s", localPort, stderrMsg)
				} else if h.verbose {
					log.Printf("[jump:%d] stderr: %s", localPort, stderrMsg)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	connWrapper := &connReadWriter{conn: conn, ctx: execCtx, cancel: cancel, graceDur: jumpCloseGrace}

	// Log successful tunnel start (non-verbose, matches TCP tunnel behavior)
	log.Printf("Jump tunnel started: :%d via %s/%s -> %s:%d",
		localPort, h.route.Namespace, podName, h.route.Target.Host, h.route.Target.Port)

	err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdin:  connWrapper,
		Stdout: connWrapper,
		Stderr: stderrWriter,
	})

	if err != nil {
		// context cancellation is normal shutdown, not an error
		if execCtx.Err() == nil {
			log.Printf("[jump:%d] Stream failed: %v", localPort, err)
			return fmt.Errorf("exec stream failed: %w", err)
		}
	}

	log.Printf("[jump:%d] Connection closed", localPort)

	return nil
}

type connReadWriter struct {
	conn     net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	graceDur time.Duration

	mu    sync.Mutex
	grace *time.Timer // set once one direction has ended
}

func (c *connReadWriter) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, io.EOF
	default:
	}

	n, err := c.conn.Read(p)
	if err == io.EOF {
		// client-go closes the remote stdin on EOF. The response can still come.
		c.startGrace()
	} else if err != nil {
		c.once.Do(c.cancel)
	}
	return n, err
}

func (c *connReadWriter) Write(p []byte) (int, error) {
	n, err := c.conn.Write(p)
	if err != nil {
		c.once.Do(c.cancel)
		return n, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grace != nil {
		c.grace.Reset(c.graceDur)
	}
	return n, nil
}

// ReadFrom is what client-go's io.Copy calls for stdout, and the only way to see
// stdout end while the remote process lives on. socat stays up for its -t period
// after the target closes, so the client has to be told here.
func (c *connReadWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(struct{ io.Writer }{c}, r)
	if err == nil {
		if tc, ok := c.conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		c.startGrace()
	}
	return n, err
}

func (c *connReadWriter) startGrace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grace == nil {
		c.grace = time.AfterFunc(c.graceDur, c.cancel)
	}
}

func (h *JumpHandler) discoverJumpPod(ctx context.Context) (podName string, containerName string, err error) {
	containerName = h.route.Via.Container

	if h.route.Via.Pod != "" {
		// Ensure pod exists (create if configured and doesn't exist)
		if err := h.ensureJumpPodExists(ctx); err != nil {
			return "", "", err
		}
		return h.route.Via.Pod, containerName, nil
	}

	svc, err := h.clientset.CoreV1().Services(h.route.Namespace).Get(
		ctx, h.route.Via.Service, metav1.GetOptions{},
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to get service %s: %w", h.route.Via.Service, err)
	}

	pod, err := k8sutil.FindReadyPod(ctx, h.clientset, h.route.Namespace, svc.Spec.Selector, h.route.Via.Service)
	if err != nil {
		return "", "", err
	}

	return pod.Name, containerName, nil
}

func (h *JumpHandler) buildForwardCommand() (string, error) {
	host := h.route.Target.Host
	port := h.route.Target.Port

	// defense-in-depth: validate host even though config validation should catch this
	if !config.IsValidTargetHost(host) {
		return "", fmt.Errorf("invalid target host %q: must be valid hostname or IP", host)
	}

	// Wrap IPv6 addresses in brackets for proper socat/nc syntax
	// e.g., 2001:db8::1 becomes [2001:db8::1] to avoid ambiguity with port separator
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}

	// prefer socat (handles binary better), use nc only if socat is not installed.
	// Deciding up front means a failed socat ends the session instead of nc silently
	// opening a new connection to the target, and exec leaves no shell in between.
	// shut-close makes socat close stdout when the target closes; the plain "-" address
	// keeps it open for all of -t. See connReadWriter.ReadFrom.
	// stderr is captured for error logging (connection refused, etc.)
	graceSeconds := int(jumpCloseGrace / time.Second)
	socatTarget := fmt.Sprintf("TCP:%s:%d", host, port)
	return fmt.Sprintf("if command -v socat >/dev/null 2>&1; then exec socat -t %d 'STDIN!!STDOUT,shut-close' %s; else exec nc %s %d; fi",
		graceSeconds, shellQuote(socatTarget), shellQuote(host), port), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureJumpPodExists returns once the jump pod is ready when via.create is configured,
// creating it if missing and replacing it if it has finished or is being deleted.
//
// Each connection runs this on its own, and other machines can share the pod, so every
// step has to cope with someone else getting there first: Create can find the pod already
// there, and Delete only applies to the exact pod that was seen to be dead.
func (h *JumpHandler) ensureJumpPodExists(ctx context.Context) error {
	// If no create config, nothing to do
	if h.route.Via.Create == nil {
		return nil
	}

	podName := h.route.Via.Pod
	namespace := h.route.Namespace
	pods := h.clientset.CoreV1().Pods(namespace)

	timeout := 60 * time.Second // default
	if h.route.Via.Create.Timeout > 0 {
		timeout = h.route.Via.Create.Timeout
	}
	deadline := time.Now().Add(timeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("jump pod %s/%s not ready after %s", namespace, podName, timeout)
		}

		pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			log.Printf("[jump] Creating jump pod %s/%s (image: %s)...", namespace, podName, h.route.Via.Create.Image)
			_, err := pods.Create(ctx, h.buildJumpPodSpec(), metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create jump pod: %w", err)
			}

		case err != nil:
			return fmt.Errorf("failed to check if pod exists: %w", err)

		case pod.DeletionTimestamp != nil:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jumpPodPollInterval):
			}

		case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
			// The name came from the user's config, so it may be someone else's pod
			if pod.Labels["app.kubernetes.io/managed-by"] != "autotunnel" {
				return fmt.Errorf("jump pod %s/%s has %s and was not created by autotunnel, delete it to continue", namespace, podName, pod.Status.Phase)
			}
			log.Printf("[jump] Replacing jump pod %s/%s (%s)", namespace, podName, pod.Status.Phase)
			err := pods.Delete(ctx, podName, metav1.DeleteOptions{Preconditions: metav1.NewUIDPreconditions(string(pod.UID))})
			if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
				return fmt.Errorf("failed to delete dead jump pod: %w", err)
			}

		default:
			err := k8sutil.WaitForPodReady(ctx, h.clientset, namespace, podName, remaining)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return fmt.Errorf("jump pod not ready: %w", err)
			}
			// It timed out, or the pod failed or went away meanwhile: check again.
			if h.verbose {
				log.Printf("[jump] Jump pod %s/%s: %v", namespace, podName, err)
			}
		}
	}
}

// buildJumpPodSpec builds the Pod manifest for the jump pod
func (h *JumpHandler) buildJumpPodSpec() *corev1.Pod {
	command := h.route.Via.Create.Command
	if len(command) == 0 {
		command = []string{"sleep", "infinity"}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.route.Via.Pod,
			Namespace: h.route.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "autotunnel-jump",
				"app.kubernetes.io/managed-by": "autotunnel",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "jump",
					Image:   h.route.Via.Create.Image,
					Command: command,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("16Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}
}

// isConnectionError checks if stderr output indicates a connection error
func isConnectionError(msg string) bool {
	msg = strings.ToLower(msg)
	errorPatterns := []string{
		"connection refused",
		"connection timed out",
		"no route to host",
		"network is unreachable",
		"host is unreachable",
		"name or service not known",
		"temporary failure in name resolution",
		"connection reset",
		"broken pipe",
	}
	for _, pattern := range errorPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
