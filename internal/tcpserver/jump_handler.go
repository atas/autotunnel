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

	connWrapper := &connReadWriter{conn: conn, ctx: execCtx, cancel: cancel}

	// Log successful tunnel start (non-verbose, matches TCP tunnel behavior)
	log.Printf("Jump tunnel started: :%d via %s/%s -> %s:%d",
		localPort, h.route.Namespace, podName, h.route.Target.Host, h.route.Target.Port)

	err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdin:  connWrapper,
		Stdout: conn,
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
	conn   net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *connReadWriter) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, io.EOF
	default:
	}

	n, err := c.conn.Read(p)
	if err != nil {
		c.once.Do(c.cancel)
	}
	return n, err
}

func (c *connReadWriter) Write(p []byte) (int, error) {
	return c.conn.Write(p)
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

	// try socat first (handles binary better), fall back to nc
	// stderr is captured for error logging (connection refused, etc.)
	return fmt.Sprintf("socat - TCP:%s:%d || nc %s %d", host, port, host, port), nil
}

// ensureJumpPodExists checks if the jump pod exists, and creates it if via.create is configured
func (h *JumpHandler) ensureJumpPodExists(ctx context.Context) error {
	// If no create config, nothing to do
	if h.route.Via.Create == nil {
		return nil
	}

	podName := h.route.Via.Pod
	namespace := h.route.Namespace

	// Check if pod already exists
	_, err := h.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		// Pod already exists
		if h.verbose {
			log.Printf("[jump] Pod %s/%s already exists", namespace, podName)
		}
		return nil
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check if pod exists: %w", err)
	}

	// Pod doesn't exist, create it
	log.Printf("[jump] Creating jump pod %s/%s (image: %s)...", namespace, podName, h.route.Via.Create.Image)

	pod := h.buildJumpPodSpec()
	_, err = h.clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Check if it was created by another request in the meantime
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create jump pod: %w", err)
	}

	// Wait for pod to be ready
	if err := h.waitForPodReady(ctx, podName); err != nil {
		return fmt.Errorf("jump pod not ready: %w", err)
	}

	log.Printf("[jump] Created jump pod %s/%s", namespace, podName)

	return nil
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

// waitForPodReady polls until the pod is ready or timeout is reached
func (h *JumpHandler) waitForPodReady(ctx context.Context, podName string) error {
	const pollInterval = 1 * time.Second

	timeout := 60 * time.Second // default
	if h.route.Via.Create != nil && h.route.Via.Create.Timeout > 0 {
		timeout = h.route.Via.Create.Timeout
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s to be ready", podName)
		}

		pod, err := h.clientset.CoreV1().Pods(h.route.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Pod was deleted, fail fast
				return fmt.Errorf("pod %s was deleted", podName)
			}
			// Transient error, continue polling
			if h.verbose {
				log.Printf("[jump] Error checking pod status: %v", err)
			}
		} else {
			// Check if pod is ready
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}

			// Check for failure states
			if pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod %s failed", podName)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}
