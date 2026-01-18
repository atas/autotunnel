package tcpserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/atas/autotunnel/internal/config"
	corev1 "k8s.io/api/core/v1"
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
	defer conn.Close()

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

	go func() {
		defer stderrReader.Close()
		buf := make([]byte, 4096)
		for {
			n, err := stderrReader.Read(buf)
			if n > 0 && h.verbose {
				log.Printf("[jump:%d] stderr: %s", localPort, string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	connWrapper := &connReadWriter{conn: conn, ctx: execCtx, cancel: cancel}

	err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdin:  connWrapper,
		Stdout: conn,
		Stderr: stderrWriter,
	})

	stderrWriter.Close()

	if err != nil {
		// context cancellation is normal shutdown, not an error
		if execCtx.Err() == nil {
			return fmt.Errorf("exec stream failed: %w", err)
		}
	}

	if h.verbose {
		log.Printf("[jump:%d] Connection closed", localPort)
	}

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
		return h.route.Via.Pod, containerName, nil
	}

	svc, err := h.clientset.CoreV1().Services(h.route.Namespace).Get(
		ctx, h.route.Via.Service, metav1.GetOptions{},
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to get service %s: %w", h.route.Via.Service, err)
	}

	pod, err := h.findReadyPod(ctx, svc.Spec.Selector)
	if err != nil {
		return "", "", err
	}

	return pod.Name, containerName, nil
}

func (h *JumpHandler) findReadyPod(ctx context.Context, selectorLabels map[string]string) (*corev1.Pod, error) {
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: selectorLabels,
	})

	pods, err := h.clientset.CoreV1().Pods(h.route.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no running pods found for service %s", h.route.Via.Service)
	}

	// Select the first ready pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pod, nil
			}
		}
	}

	return &pods.Items[0], nil
}

func (h *JumpHandler) buildForwardCommand() (string, error) {
	host := h.route.Target.Host
	port := h.route.Target.Port

	// defense-in-depth: validate host even though config validation should catch this
	if !config.IsValidTargetHost(host) {
		return "", fmt.Errorf("invalid target host %q: must be valid hostname or IP", host)
	}

	// try socat first (handles binary better), fall back to nc
	return fmt.Sprintf("socat - TCP:%s:%d 2>/dev/null || nc %s %d", host, port, host, port), nil
}
