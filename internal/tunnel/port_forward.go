package tunnel

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/atas/autotunnel/internal/k8sutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

func (t *Tunnel) startPortForward(ctx context.Context) error {
	podName, targetPort, err := t.discoverTargetPod(ctx)
	if err != nil {
		return err
	}

	fw, errChan, err := t.createPortForwarder(podName, targetPort)
	if err != nil {
		return err
	}

	return t.waitForReady(ctx, fw, errChan)
}

// discoverTargetPod figures out which pod to connect to.
// With pod: config, we use it directly. With service: config, we look up the
// service's selector labels and find a ready pod that matches.
func (t *Tunnel) discoverTargetPod(ctx context.Context) (podName string, port int, err error) {
	port = t.config.Port

	if t.config.Pod != "" {
		if t.verbose {
			log.Printf("[%s] Direct pod targeting: %s/%s port %d", t.hostname, t.config.Namespace, t.config.Pod, port)
		}
		return t.config.Pod, port, nil
	}

	svc, err := t.clientset.CoreV1().Services(t.config.Namespace).Get(
		ctx, t.config.Service, metav1.GetOptions{},
	)
	if err != nil {
		t.setFailed(err)
		return "", 0, fmt.Errorf("failed to get service: %w", err)
	}

	// K8s services can map ports (e.g. service:80 -> container:8080),
	// so we need to resolve to the actual container port
	var targetPortName string
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == t.config.Port {
			if p.TargetPort.IntVal != 0 {
				port = int(p.TargetPort.IntVal)
			} else if p.TargetPort.StrVal != "" {
				// named port - need the pod spec to resolve it
				targetPortName = p.TargetPort.StrVal
			}
			break
		}
	}

	targetPod, err := k8sutil.FindReadyPod(ctx, t.clientset, t.config.Namespace, svc.Spec.Selector, t.config.Service)
	if err != nil {
		t.setFailed(err)
		return "", 0, err
	}

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

func (t *Tunnel) createPortForwarder(podName string, targetPort int) (*portforward.PortForwarder, chan error, error) {
	req := t.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(t.config.Namespace).
		Name(podName).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(t.restConfig)
	if err != nil {
		t.setFailed(err)
		return nil, nil, fmt.Errorf("failed to create round tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	t.stopChan = make(chan struct{})
	t.readyChan = make(chan struct{})

	// port 0 = let the OS pick an available port
	ports := []string{fmt.Sprintf("0:%d", targetPort)}

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

	// ForwardPorts blocks, so run it in background and signal errors via channel
	errChan := make(chan error, 1)
	go func() {
		errChan <- fw.ForwardPorts()
	}()

	return fw, errChan, nil
}

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

	case <-time.After(PortForwardReadyTimeout):
		close(t.stopChan)
		err := fmt.Errorf("timeout waiting for port forward to be ready")
		t.setFailed(err)
		return err
	}
}

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

	// the port-forward can die anytime (pod restart, network issues, etc)
	go t.monitorErrors(errChan)

	return nil
}

func (t *Tunnel) monitorErrors(errChan chan error) {
	if err := <-errChan; err != nil {
		log.Printf("[%s] Port forward error: %v", t.hostname, err)
		t.setFailed(err)
	}
}

func (t *Tunnel) setFailed(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastError = err
	t.state = StateFailed
}
