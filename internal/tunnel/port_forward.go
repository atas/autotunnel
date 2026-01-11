package tunnel

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// startPortForward establishes the port-forward connection using client-go
func (t *Tunnel) startPortForward(ctx context.Context) error {
	// Discover target pod and port
	podName, targetPort, err := t.discoverTargetPod(ctx)
	if err != nil {
		return err
	}

	// Create port forwarder
	fw, errChan, err := t.createPortForwarder(podName, targetPort)
	if err != nil {
		return err
	}

	// Wait for ready and handle lifecycle
	return t.waitForReady(ctx, fw, errChan)
}

// discoverTargetPod returns the pod name and resolved port for port-forwarding.
// For direct pod targeting, it returns the configured pod name.
// For service targeting, it discovers a ready pod via the service selector.
func (t *Tunnel) discoverTargetPod(ctx context.Context) (podName string, port int, err error) {
	port = t.config.Port

	if t.config.Pod != "" {
		// Direct pod targeting - skip service lookup and pod discovery
		if t.verbose {
			log.Printf("[%s] Direct pod targeting: %s/%s port %d", t.hostname, t.config.Namespace, t.config.Pod, port)
		}
		return t.config.Pod, port, nil
	}

	// Service targeting - lookup service and discover pod
	svc, err := t.clientset.CoreV1().Services(t.config.Namespace).Get(
		ctx, t.config.Service, metav1.GetOptions{},
	)
	if err != nil {
		t.setFailed(err)
		return "", 0, fmt.Errorf("failed to get service: %w", err)
	}

	// Resolve service port to target port (container port)
	var targetPortName string
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == t.config.Port {
			if p.TargetPort.IntVal != 0 {
				port = int(p.TargetPort.IntVal)
			} else if p.TargetPort.StrVal != "" {
				// Named port - will resolve after finding the pod
				targetPortName = p.TargetPort.StrVal
			}
			break
		}
	}

	// Find a ready pod using the service selector
	targetPod, err := t.findReadyPod(ctx, svc.Spec.Selector)
	if err != nil {
		return "", 0, err
	}

	// Resolve named port if needed
	if targetPortName != "" {
		port, err = t.resolveNamedPort(targetPod, targetPortName)
		if err != nil {
			return "", 0, err
		}
	}

	if t.verbose {
		log.Printf("[%s] Forwarding to pod %s/%s port %d (via service %s)", t.hostname, t.config.Namespace, targetPod.Name, port, t.config.Service)
	}

	return targetPod.Name, port, nil
}

// findReadyPod finds a ready pod matching the given selector labels
func (t *Tunnel) findReadyPod(ctx context.Context, selectorLabels map[string]string) (*corev1.Pod, error) {
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: selectorLabels,
	})

	pods, err := t.clientset.CoreV1().Pods(t.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		t.setFailed(err)
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		err := fmt.Errorf("no running pods found for service %s", t.config.Service)
		t.setFailed(err)
		return nil, err
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

	// Fallback to first running pod even if not ready
	return &pods.Items[0], nil
}

// resolveNamedPort finds the container port for a named port
func (t *Tunnel) resolveNamedPort(pod *corev1.Pod, portName string) (int, error) {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == portName {
				return int(port.ContainerPort), nil
			}
		}
	}

	err := fmt.Errorf("could not resolve named port %q in pod %s", portName, pod.Name)
	t.setFailed(err)
	return 0, err
}

// createPortForwarder creates and starts the SPDY port forwarder
func (t *Tunnel) createPortForwarder(podName string, targetPort int) (*portforward.PortForwarder, chan error, error) {
	// Build the port-forward URL
	req := t.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(t.config.Namespace).
		Name(podName).
		SubResource("portforward")

	// Create the SPDY transport and dialer
	transport, upgrader, err := spdy.RoundTripperFor(t.restConfig)
	if err != nil {
		t.setFailed(err)
		return nil, nil, fmt.Errorf("failed to create round tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	// Set up channels for port-forward lifecycle
	t.stopChan = make(chan struct{})
	t.readyChan = make(chan struct{})

	// Use port 0 to get a random available local port
	ports := []string{fmt.Sprintf("0:%d", targetPort)}

	// Configure output writers
	var out, errOut io.Writer = io.Discard, io.Discard
	if t.verbose {
		out = os.Stdout
		errOut = os.Stderr
	}

	fw, err := portforward.New(dialer, ports, t.stopChan, t.readyChan, out, errOut)
	if err != nil {
		t.setFailed(err)
		return nil, nil, fmt.Errorf("failed to create port forwarder: %w", err)
	}

	// Start port forwarding in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- fw.ForwardPorts()
	}()

	return fw, errChan, nil
}

// waitForReady waits for the port forwarder to be ready or fail
func (t *Tunnel) waitForReady(ctx context.Context, fw *portforward.PortForwarder, errChan chan error) error {
	select {
	case <-t.readyChan:
		return t.handleReady(fw, errChan)

	case err := <-errChan:
		t.setFailed(err)
		return fmt.Errorf("port forward failed: %w", err)

	case <-ctx.Done():
		close(t.stopChan)
		t.mu.Lock()
		t.state = StateIdle
		t.mu.Unlock()
		return ctx.Err()

	case <-time.After(30 * time.Second):
		close(t.stopChan)
		err := fmt.Errorf("timeout waiting for port forward to be ready")
		t.setFailed(err)
		return err
	}
}

// handleReady processes the successful ready state and starts error monitoring
func (t *Tunnel) handleReady(fw *portforward.PortForwarder, errChan chan error) error {
	forwardedPorts, err := fw.GetPorts()
	if err != nil {
		t.setFailed(err)
		return fmt.Errorf("failed to get forwarded ports: %w", err)
	}

	if len(forwardedPorts) == 0 {
		err := fmt.Errorf("no ports were forwarded")
		t.setFailed(err)
		return err
	}

	t.mu.Lock()
	t.localPort = int(forwardedPorts[0].Local)
	t.state = StateRunning
	t.mu.Unlock()

	// Log tunnel started
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
	go t.monitorErrors(errChan)

	return nil
}

// monitorErrors watches for port-forward errors and updates tunnel state
func (t *Tunnel) monitorErrors(errChan chan error) {
	if err := <-errChan; err != nil {
		log.Printf("[%s] Port forward error: %v", t.hostname, err)
		t.setFailed(err)
	}
}

// setFailed sets the tunnel state to failed with the given error
func (t *Tunnel) setFailed(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastError = err
	t.state = StateFailed
}
