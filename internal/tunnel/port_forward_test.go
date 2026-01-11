package tunnel

import (
	"context"
	"testing"

	"github.com/atas/lazyfwd/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

// ============================================================================
// resolveNamedPort tests (pure logic, no mocks)
// ============================================================================

func TestResolveNamedPort_Found(t *testing.T) {
	tunnel := &Tunnel{}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8080},
						{Name: "metrics", ContainerPort: 9090},
					},
				},
			},
		},
	}

	port, err := tunnel.resolveNamedPort(pod, "http")
	if err != nil {
		t.Fatalf("resolveNamedPort() error = %v", err)
	}
	if port != 8080 {
		t.Errorf("resolveNamedPort() = %d, want 8080", port)
	}
}

func TestResolveNamedPort_NotFound(t *testing.T) {
	tunnel := &Tunnel{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8080},
					},
				},
			},
		},
	}

	_, err := tunnel.resolveNamedPort(pod, "nonexistent")
	if err == nil {
		t.Fatal("resolveNamedPort() expected error, got nil")
	}
}

func TestResolveNamedPort_MultipleContainers(t *testing.T) {
	tunnel := &Tunnel{}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "sidecar",
					Ports: []corev1.ContainerPort{
						{Name: "proxy", ContainerPort: 15000},
					},
				},
				{
					Name: "main",
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8080},
					},
				},
			},
		},
	}

	// Should find port in second container
	port, err := tunnel.resolveNamedPort(pod, "http")
	if err != nil {
		t.Fatalf("resolveNamedPort() error = %v", err)
	}
	if port != 8080 {
		t.Errorf("resolveNamedPort() = %d, want 8080", port)
	}

	// Should find port in first container
	port, err = tunnel.resolveNamedPort(pod, "proxy")
	if err != nil {
		t.Fatalf("resolveNamedPort() error = %v", err)
	}
	if port != 15000 {
		t.Errorf("resolveNamedPort() = %d, want 15000", port)
	}
}

func TestResolveNamedPort_EmptyContainers(t *testing.T) {
	tunnel := &Tunnel{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{},
		},
	}

	_, err := tunnel.resolveNamedPort(pod, "http")
	if err == nil {
		t.Fatal("resolveNamedPort() expected error for empty containers")
	}
}

// ============================================================================
// findReadyPod tests (with fake K8s clientset)
// ============================================================================

func TestFindReadyPod_PrefersReadyPod(t *testing.T) {
	ctx := context.Background()

	// Create two pods: one running but not ready, one ready
	notReadyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-not-ready",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-ready",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(notReadyPod, readyPod)

	tunnel := &Tunnel{
		clientset: fakeClient,
		config:    config.K8sRouteConfig{Namespace: "test-ns", Service: "test-svc"},
	}

	pod, err := tunnel.findReadyPod(ctx, map[string]string{"app": "test"})
	if err != nil {
		t.Fatalf("findReadyPod() error = %v", err)
	}
	if pod.Name != "pod-ready" {
		t.Errorf("findReadyPod() selected %q, want %q", pod.Name, "pod-ready")
	}
}

func TestFindReadyPod_FallsBackToRunning(t *testing.T) {
	ctx := context.Background()

	// Only create pods that are running but not ready
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-running",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(runningPod)

	tunnel := &Tunnel{
		clientset: fakeClient,
		config:    config.K8sRouteConfig{Namespace: "test-ns", Service: "test-svc"},
	}

	pod, err := tunnel.findReadyPod(ctx, map[string]string{"app": "test"})
	if err != nil {
		t.Fatalf("findReadyPod() error = %v", err)
	}
	if pod.Name != "pod-running" {
		t.Errorf("findReadyPod() selected %q, want %q", pod.Name, "pod-running")
	}
}

func TestFindReadyPod_NoPods(t *testing.T) {
	ctx := context.Background()

	fakeClient := fake.NewSimpleClientset()

	tunnel := &Tunnel{
		clientset: fakeClient,
		config:    config.K8sRouteConfig{Namespace: "test-ns", Service: "test-svc"},
	}

	_, err := tunnel.findReadyPod(ctx, map[string]string{"app": "test"})
	if err == nil {
		t.Fatal("findReadyPod() expected error for no pods, got nil")
	}
}

func TestFindReadyPod_IgnoresNonMatchingLabels(t *testing.T) {
	ctx := context.Background()

	// Create a pod with different labels
	differentPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "different-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "other"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(differentPod)

	tunnel := &Tunnel{
		clientset: fakeClient,
		config:    config.K8sRouteConfig{Namespace: "test-ns", Service: "test-svc"},
	}

	_, err := tunnel.findReadyPod(ctx, map[string]string{"app": "test"})
	if err == nil {
		t.Fatal("findReadyPod() expected error when no matching pods, got nil")
	}
}

// ============================================================================
// discoverTargetPod tests (with fake K8s clientset)
// ============================================================================

func TestDiscoverTargetPod_DirectPodMode(t *testing.T) {
	ctx := context.Background()

	tunnel := &Tunnel{
		hostname: "test.localhost",
		config: config.K8sRouteConfig{
			Namespace: "test-ns",
			Pod:       "my-pod",
			Port:      8080,
		},
		verbose: false,
	}

	podName, port, err := tunnel.discoverTargetPod(ctx)
	if err != nil {
		t.Fatalf("discoverTargetPod() error = %v", err)
	}
	if podName != "my-pod" {
		t.Errorf("discoverTargetPod() podName = %q, want %q", podName, "my-pod")
	}
	if port != 8080 {
		t.Errorf("discoverTargetPod() port = %d, want %d", port, 8080)
	}
}

func TestDiscoverTargetPod_ServiceMode_NumericPort(t *testing.T) {
	ctx := context.Background()

	// Create a service with numeric target port
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
				},
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(svc, pod)

	tunnel := &Tunnel{
		hostname:  "test.localhost",
		clientset: fakeClient,
		config: config.K8sRouteConfig{
			Namespace: "test-ns",
			Service:   "test-svc",
			Port:      80,
		},
		verbose: false,
	}

	podName, port, err := tunnel.discoverTargetPod(ctx)
	if err != nil {
		t.Fatalf("discoverTargetPod() error = %v", err)
	}
	if podName != "test-pod" {
		t.Errorf("discoverTargetPod() podName = %q, want %q", podName, "test-pod")
	}
	if port != 8080 {
		t.Errorf("discoverTargetPod() port = %d, want %d (target port)", port, 8080)
	}
}

func TestDiscoverTargetPod_ServiceMode_NamedPort(t *testing.T) {
	ctx := context.Background()

	// Create a service with named target port
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Port:       443,
					TargetPort: intstr.FromString("https"),
				},
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Ports: []corev1.ContainerPort{
						{Name: "https", ContainerPort: 8443},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(svc, pod)

	tunnel := &Tunnel{
		hostname:  "test.localhost",
		clientset: fakeClient,
		config: config.K8sRouteConfig{
			Namespace: "test-ns",
			Service:   "test-svc",
			Port:      443,
		},
		verbose: false,
	}

	podName, port, err := tunnel.discoverTargetPod(ctx)
	if err != nil {
		t.Fatalf("discoverTargetPod() error = %v", err)
	}
	if podName != "test-pod" {
		t.Errorf("discoverTargetPod() podName = %q, want %q", podName, "test-pod")
	}
	if port != 8443 {
		t.Errorf("discoverTargetPod() port = %d, want %d (resolved from named port)", port, 8443)
	}
}

func TestDiscoverTargetPod_ServiceNotFound(t *testing.T) {
	ctx := context.Background()

	fakeClient := fake.NewSimpleClientset() // No services

	tunnel := &Tunnel{
		hostname:  "test.localhost",
		clientset: fakeClient,
		config: config.K8sRouteConfig{
			Namespace: "test-ns",
			Service:   "nonexistent-svc",
			Port:      80,
		},
		verbose: false,
	}

	_, _, err := tunnel.discoverTargetPod(ctx)
	if err == nil {
		t.Fatal("discoverTargetPod() expected error for nonexistent service, got nil")
	}
}

func TestDiscoverTargetPod_NoPodsForService(t *testing.T) {
	ctx := context.Background()

	// Create service but no pods
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "test"},
			Ports: []corev1.ServicePort{
				{
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
				},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(svc)

	tunnel := &Tunnel{
		hostname:  "test.localhost",
		clientset: fakeClient,
		config: config.K8sRouteConfig{
			Namespace: "test-ns",
			Service:   "test-svc",
			Port:      80,
		},
		verbose: false,
	}

	_, _, err := tunnel.discoverTargetPod(ctx)
	if err == nil {
		t.Fatal("discoverTargetPod() expected error for no pods, got nil")
	}
}

// ============================================================================
// setFailed tests
// ============================================================================

func TestSetFailed(t *testing.T) {
	tunnel := &Tunnel{state: StateStarting}
	testErr := &testError{msg: "connection failed"}

	tunnel.setFailed(testErr)

	if tunnel.state != StateFailed {
		t.Errorf("state = %v, want %v", tunnel.state, StateFailed)
	}
	if tunnel.lastError != testErr {
		t.Errorf("lastError = %v, want %v", tunnel.lastError, testErr)
	}
}
