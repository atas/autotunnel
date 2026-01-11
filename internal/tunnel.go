package internal

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/atas/lazyfwd/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// TunnelState represents the current state of a tunnel
type TunnelState int

const (
	TunnelStateIdle TunnelState = iota
	TunnelStateStarting
	TunnelStateRunning
	TunnelStateStopping
	TunnelStateFailed
)

func (s TunnelState) String() string {
	switch s {
	case TunnelStateIdle:
		return "idle"
	case TunnelStateStarting:
		return "starting"
	case TunnelStateRunning:
		return "running"
	case TunnelStateStopping:
		return "stopping"
	case TunnelStateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Tunnel manages a single port-forward connection to a Kubernetes service or pod
type Tunnel struct {
	mu sync.RWMutex

	// Configuration
	hostname   string
	config     config.K8sRouteConfig
	listenAddr string
	verbose    bool

	// Shared k8s resources (from Manager)
	clientset  *kubernetes.Clientset
	restConfig *rest.Config

	// State
	state      TunnelState
	localPort  int
	lastAccess time.Time

	// Port-forward resources
	stopChan  chan struct{}
	readyChan chan struct{}

	// Error tracking
	lastError error
}

// NewTunnel creates a new tunnel instance
func NewTunnel(hostname string, cfg config.K8sRouteConfig, clientset *kubernetes.Clientset, restConfig *rest.Config, listenAddr string, verbose bool) *Tunnel {
	return &Tunnel{
		hostname:   hostname,
		config:     cfg,
		clientset:  clientset,
		restConfig: restConfig,
		listenAddr: listenAddr,
		verbose:    verbose,
		state:      TunnelStateIdle,
		lastAccess: time.Now(),
	}
}

// Start initiates the port-forward connection
func (t *Tunnel) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.state == TunnelStateRunning {
		t.mu.Unlock()
		return nil
	}
	t.state = TunnelStateStarting
	t.mu.Unlock()

	return t.startPortForward(ctx)
}

// Stop gracefully terminates the port-forward
func (t *Tunnel) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.state != TunnelStateRunning {
		return
	}

	t.state = TunnelStateStopping
	if t.stopChan != nil {
		close(t.stopChan)
	}
	t.state = TunnelStateIdle
}

// LocalPort returns the local port the tunnel is listening on
func (t *Tunnel) LocalPort() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.localPort
}

// Touch updates the last access time
func (t *Tunnel) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastAccess = time.Now()
}

// IdleDuration returns how long the tunnel has been idle
func (t *Tunnel) IdleDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Since(t.lastAccess)
}

// IsRunning returns true if the tunnel is active
func (t *Tunnel) IsRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state == TunnelStateRunning
}

// State returns the current tunnel state
func (t *Tunnel) State() TunnelState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// Hostname returns the hostname this tunnel serves
func (t *Tunnel) Hostname() string {
	return t.hostname
}

// Scheme returns the scheme (http/https) for X-Forwarded-Proto header
func (t *Tunnel) Scheme() string {
	if t.config.Scheme == "" {
		return "http"
	}
	return t.config.Scheme
}

// startPortForward establishes the port-forward connection using client-go
func (t *Tunnel) startPortForward(ctx context.Context) error {
	// Use shared clientset and restConfig from Manager
	clientset := t.clientset
	restConfig := t.restConfig

	// Variables to hold the target pod name and port
	var targetPodName string
	targetPort := t.config.Port

	if t.config.Pod != "" {
		// Direct pod targeting - skip service lookup and pod discovery
		targetPodName = t.config.Pod
		if t.verbose {
			log.Printf("[%s] Direct pod targeting: %s/%s port %d", t.hostname, t.config.Namespace, targetPodName, targetPort)
		}
	} else {
		// Service targeting - lookup service and discover pod
		// Step 3: Get the service to find its selector and port mapping
		svc, err := clientset.CoreV1().Services(t.config.Namespace).Get(
			ctx, t.config.Service, metav1.GetOptions{},
		)
		if err != nil {
			t.mu.Lock()
			t.lastError = err
			t.state = TunnelStateFailed
			t.mu.Unlock()
			return fmt.Errorf("failed to get service: %w", err)
		}

		// Resolve service port to target port (container port)
		// This allows users to specify service ports (e.g., 443) which get mapped
		// to the actual container port (e.g., 8443)
		var targetPortName string
		for _, p := range svc.Spec.Ports {
			if int(p.Port) == t.config.Port {
				if p.TargetPort.IntVal != 0 {
					targetPort = int(p.TargetPort.IntVal)
				} else if p.TargetPort.StrVal != "" {
					// Named port - will resolve after finding the pod
					targetPortName = p.TargetPort.StrVal
				}
				break
			}
		}

		// Step 4: Find a ready pod using the service selector
		selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
			MatchLabels: svc.Spec.Selector,
		})

		pods, err := clientset.CoreV1().Pods(t.config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			t.mu.Lock()
			t.lastError = err
			t.state = TunnelStateFailed
			t.mu.Unlock()
			return fmt.Errorf("failed to list pods: %w", err)
		}

		if len(pods.Items) == 0 {
			err := fmt.Errorf("no running pods found for service %s", t.config.Service)
			t.mu.Lock()
			t.lastError = err
			t.state = TunnelStateFailed
			t.mu.Unlock()
			return err
		}

		// Select the first ready pod
		var targetPod *corev1.Pod
		for i := range pods.Items {
			pod := &pods.Items[i]
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					targetPod = pod
					break
				}
			}
			if targetPod != nil {
				break
			}
		}

		if targetPod == nil {
			// Fallback to first running pod even if not ready
			targetPod = &pods.Items[0]
		}

		// Resolve named port if needed
		if targetPortName != "" {
			resolved := false
			for _, container := range targetPod.Spec.Containers {
				for _, port := range container.Ports {
					if port.Name == targetPortName {
						targetPort = int(port.ContainerPort)
						resolved = true
						break
					}
				}
				if resolved {
					break
				}
			}
			if !resolved {
				err := fmt.Errorf("could not resolve named port %q in pod %s", targetPortName, targetPod.Name)
				t.mu.Lock()
				t.lastError = err
				t.state = TunnelStateFailed
				t.mu.Unlock()
				return err
			}
		}

		targetPodName = targetPod.Name
		if t.verbose {
			log.Printf("[%s] Forwarding to pod %s/%s port %d (via service %s)", t.hostname, t.config.Namespace, targetPodName, targetPort, t.config.Service)
		}
	}

	// Step 5: Build the port-forward URL
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(t.config.Namespace).
		Name(targetPodName).
		SubResource("portforward")

	// Step 6: Create the SPDY transport and dialer
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.mu.Lock()
		t.lastError = err
		t.state = TunnelStateFailed
		t.mu.Unlock()
		return fmt.Errorf("failed to create round tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	// Step 7: Set up channels for port-forward lifecycle
	t.stopChan = make(chan struct{})
	t.readyChan = make(chan struct{})

	// Use port 0 to get a random available local port, forward to resolved targetPort
	ports := []string{fmt.Sprintf("0:%d", targetPort)}

	// Step 8: Create the port forwarder
	var out, errOut io.Writer = io.Discard, io.Discard
	if t.verbose {
		out = os.Stdout
		errOut = os.Stderr
	}

	fw, err := portforward.New(dialer, ports, t.stopChan, t.readyChan, out, errOut)
	if err != nil {
		t.mu.Lock()
		t.lastError = err
		t.state = TunnelStateFailed
		t.mu.Unlock()
		return fmt.Errorf("failed to create port forwarder: %w", err)
	}

	// Step 9: Start port forwarding in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- fw.ForwardPorts()
	}()

	// Step 10: Wait for ready or error
	select {
	case <-t.readyChan:
		// Port forward is ready
		forwardedPorts, err := fw.GetPorts()
		if err != nil {
			t.mu.Lock()
			t.lastError = err
			t.state = TunnelStateFailed
			t.mu.Unlock()
			return fmt.Errorf("failed to get forwarded ports: %w", err)
		}

		if len(forwardedPorts) == 0 {
			err := fmt.Errorf("no ports were forwarded")
			t.mu.Lock()
			t.lastError = err
			t.state = TunnelStateFailed
			t.mu.Unlock()
			return err
		}

		t.mu.Lock()
		t.localPort = int(forwardedPorts[0].Local)
		t.state = TunnelStateRunning
		t.mu.Unlock()

		scheme := t.config.Scheme
		if scheme == "" {
			scheme = "http"
		}
		target := t.config.Service
		if t.config.Pod != "" {
			target = "pod/" + t.config.Pod
		}
		log.Printf("Tunnel started: %s://%s%s -> %s/%s:%d",
			scheme, t.hostname, t.listenAddr, t.config.Namespace, target, t.config.Port)

		// Start goroutine to monitor for errors
		go func() {
			if err := <-errChan; err != nil {
				log.Printf("[%s] Port forward error: %v", t.hostname, err)
				t.mu.Lock()
				t.state = TunnelStateFailed
				t.lastError = err
				t.mu.Unlock()
			}
		}()

		return nil

	case err := <-errChan:
		t.mu.Lock()
		t.lastError = err
		t.state = TunnelStateFailed
		t.mu.Unlock()
		return fmt.Errorf("port forward failed: %w", err)

	case <-ctx.Done():
		close(t.stopChan)
		t.mu.Lock()
		t.state = TunnelStateIdle
		t.mu.Unlock()
		return ctx.Err()

	case <-time.After(30 * time.Second):
		close(t.stopChan)
		err := fmt.Errorf("timeout waiting for port forward to be ready")
		t.mu.Lock()
		t.lastError = err
		t.state = TunnelStateFailed
		t.mu.Unlock()
		return err
	}
}
