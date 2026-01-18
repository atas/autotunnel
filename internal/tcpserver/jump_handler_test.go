package tcpserver

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/k8sutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNewJumpHandler(t *testing.T) {
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Service: "jump-svc",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}
	kubeconfig := []string{"/path/to/kubeconfig"}
	clientset := fake.NewSimpleClientset()
	verbose := true

	handler := NewJumpHandler(route, kubeconfig, clientset, nil, verbose)

	if handler == nil {
		t.Fatal("NewJumpHandler returned nil")
	}
	if handler.route.Namespace != "test-ns" {
		t.Errorf("Expected namespace 'test-ns', got %q", handler.route.Namespace)
	}
	if handler.route.Via.Service != "jump-svc" {
		t.Errorf("Expected via service 'jump-svc', got %q", handler.route.Via.Service)
	}
	if handler.route.Target.Host != "database.internal" {
		t.Errorf("Expected target host 'database.internal', got %q", handler.route.Target.Host)
	}
	if handler.route.Target.Port != 5432 {
		t.Errorf("Expected target port 5432, got %d", handler.route.Target.Port)
	}
	if handler.verbose != true {
		t.Errorf("Expected verbose true, got %v", handler.verbose)
	}
}

func TestJumpHandler_discoverJumpPod_DirectPod(t *testing.T) {
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod:       "my-jump-pod",
			Container: "main",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	clientset := fake.NewSimpleClientset()
	handler := NewJumpHandler(route, nil, clientset, nil, false)

	podName, containerName, err := handler.discoverJumpPod(context.Background())
	if err != nil {
		t.Fatalf("discoverJumpPod failed: %v", err)
	}

	if podName != "my-jump-pod" {
		t.Errorf("Expected pod name 'my-jump-pod', got %q", podName)
	}
	if containerName != "main" {
		t.Errorf("Expected container name 'main', got %q", containerName)
	}
}

func TestJumpHandler_discoverJumpPod_ViaService(t *testing.T) {
	// Create a fake service and pod
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "jump-svc",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "jump-host",
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "jump-pod-abc123",
			Namespace: "test-ns",
			Labels: map[string]string{
				"app": "jump-host",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(svc, pod)

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Service:   "jump-svc",
			Container: "sidecar",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	podName, containerName, err := handler.discoverJumpPod(context.Background())
	if err != nil {
		t.Fatalf("discoverJumpPod failed: %v", err)
	}

	if podName != "jump-pod-abc123" {
		t.Errorf("Expected pod name 'jump-pod-abc123', got %q", podName)
	}
	if containerName != "sidecar" {
		t.Errorf("Expected container name 'sidecar', got %q", containerName)
	}
}

func TestJumpHandler_discoverJumpPod_ServiceNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Service: "nonexistent-svc",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	_, _, err := handler.discoverJumpPod(context.Background())
	if err == nil {
		t.Fatal("Expected error for nonexistent service, got nil")
	}
}

func TestJumpHandler_discoverJumpPod_NoPodsFound(t *testing.T) {
	// Create service but no matching pods
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "jump-svc",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "jump-host",
			},
		},
	}

	clientset := fake.NewSimpleClientset(svc)

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Service: "jump-svc",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	_, _, err := handler.discoverJumpPod(context.Background())
	if err == nil {
		t.Fatal("Expected error for no pods found, got nil")
	}
}

func TestJumpHandler_findReadyPod_SelectsReady(t *testing.T) {
	// Create multiple pods, only one is ready
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ready-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "jump"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	notReadyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-ready-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "jump"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	clientset := fake.NewSimpleClientset(readyPod, notReadyPod)

	pod, err := k8sutil.FindReadyPod(context.Background(), clientset, "test-ns", map[string]string{"app": "jump"}, "svc")
	if err != nil {
		t.Fatalf("FindReadyPod failed: %v", err)
	}

	if pod.Name != "ready-pod" {
		t.Errorf("Expected 'ready-pod', got %q", pod.Name)
	}
}

func TestJumpHandler_findReadyPod_FallbackToRunning(t *testing.T) {
	// Create a running but not-ready pod
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "jump"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	clientset := fake.NewSimpleClientset(runningPod)

	pod, err := k8sutil.FindReadyPod(context.Background(), clientset, "test-ns", map[string]string{"app": "jump"}, "svc")
	if err != nil {
		t.Fatalf("FindReadyPod failed: %v", err)
	}

	// Should fall back to the running pod even though not ready
	if pod.Name != "running-pod" {
		t.Errorf("Expected 'running-pod' as fallback, got %q", pod.Name)
	}
}

func TestJumpHandler_buildForwardCommand(t *testing.T) {
	tests := []struct {
		name       string
		targetHost string
		targetPort int
		wantSocat  string
		wantNc     string
	}{
		{
			name:       "standard host and port",
			targetHost: "database.internal",
			targetPort: 5432,
			wantSocat:  "socat - TCP:database.internal:5432",
			wantNc:     "nc database.internal 5432",
		},
		{
			name:       "localhost target",
			targetHost: "127.0.0.1",
			targetPort: 3306,
			wantSocat:  "socat - TCP:127.0.0.1:3306",
			wantNc:     "nc 127.0.0.1 3306",
		},
		{
			name:       "RDS endpoint",
			targetHost: "mydb.cluster-xyz.us-east-1.rds.amazonaws.com",
			targetPort: 5432,
			wantSocat:  "socat - TCP:mydb.cluster-xyz.us-east-1.rds.amazonaws.com:5432",
			wantNc:     "nc mydb.cluster-xyz.us-east-1.rds.amazonaws.com 5432",
		},
		{
			name:       "IPv6 address",
			targetHost: "2001:db8::1",
			targetPort: 5432,
			wantSocat:  "socat - TCP:[2001:db8::1]:5432",
			wantNc:     "nc [2001:db8::1] 5432",
		},
		{
			name:       "IPv6 loopback",
			targetHost: "::1",
			targetPort: 3306,
			wantSocat:  "socat - TCP:[::1]:3306",
			wantNc:     "nc [::1] 3306",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := config.JumpRouteConfig{
				Target: config.TargetConfig{
					Host: tt.targetHost,
					Port: tt.targetPort,
				},
			}
			handler := NewJumpHandler(route, nil, nil, nil, false)

			cmd, err := handler.buildForwardCommand()
			if err != nil {
				t.Fatalf("buildForwardCommand() error = %v", err)
			}

			// Command should contain both socat and nc fallback
			// (stderr captured for error logging, not suppressed)
			expectedCmd := tt.wantSocat + " || " + tt.wantNc
			if cmd != expectedCmd {
				t.Errorf("buildForwardCommand() = %q, want %q", cmd, expectedCmd)
			}
		})
	}
}

func TestIsConnectionError(t *testing.T) {
	tests := []struct {
		msg      string
		expected bool
	}{
		// Should match
		{"Connection refused", true},
		{"connection refused", true},
		{"ECONNREFUSED: Connection refused", true},
		{"Connection timed out", true},
		{"No route to host", true},
		{"Network is unreachable", true},
		{"Host is unreachable", true},
		{"Name or service not known", true},
		{"Temporary failure in name resolution", true},
		{"Connection reset by peer", true},
		{"Broken pipe", true},

		// Should not match
		{"Connected successfully", false},
		{"Data transferred", false},
		{"", false},
		{"socat version 1.7.4", false},
		{"Normal stderr output", false},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if got := isConnectionError(tt.msg); got != tt.expected {
				t.Errorf("isConnectionError(%q) = %v, want %v", tt.msg, got, tt.expected)
			}
		})
	}
}

func TestConnReadWriter_Read(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	testData := []byte("hello world")
	go func() {
		_, _ = client.Write(testData)
		client.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
	}

	buf := make([]byte, len(testData))
	n, err := crw.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Read %d bytes, want %d", n, len(testData))
	}
	if string(buf[:n]) != string(testData) {
		t.Errorf("Read %q, want %q", buf[:n], testData)
	}
}

func TestConnReadWriter_ContextCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
	}

	// Cancel context before reading
	cancel()

	buf := make([]byte, 10)
	n, err := crw.Read(buf)

	if n != 0 {
		t.Errorf("Read returned %d bytes, want 0", n)
	}
	if err != io.EOF {
		t.Errorf("Read error = %v, want io.EOF", err)
	}
}

func TestConnReadWriter_Write(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
	}

	testData := []byte("test message")

	// Read in goroutine
	var received []byte
	var readErr error
	done := make(chan struct{})
	go func() {
		received, readErr = io.ReadAll(client)
		close(done)
	}()

	n, err := crw.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Write returned %d, want %d", n, len(testData))
	}

	// Close to signal EOF
	server.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Read timed out")
	}

	if readErr != nil {
		t.Fatalf("Read error: %v", readErr)
	}
	if string(received) != string(testData) {
		t.Errorf("Received %q, want %q", received, testData)
	}
}

func TestConnReadWriter_ReadErrorCancelsContext(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
	}

	// Close client side to cause read error
	client.Close()

	buf := make([]byte, 10)
	_, err := crw.Read(buf)

	if err == nil {
		t.Error("Expected error on read after close, got nil")
	}

	// Context should be cancelled after read error
	select {
	case <-ctx.Done():
		// Good - context was cancelled
	default:
		t.Error("Context should be cancelled after read error")
	}
}

func TestConnReadWriter_OnlyOneCancelOnMultipleErrors(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	client.Close() // Close immediately to cause errors

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
		once:   sync.Once{},
	}

	buf := make([]byte, 10)

	// Multiple reads should not panic (once ensures single cancel)
	for i := 0; i < 3; i++ {
		_, _ = crw.Read(buf)
	}

	// If we reach here without panic, sync.Once worked correctly
}

func TestJumpHandler_HandleConnection_NoRestConfig(t *testing.T) {
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "jump-pod",
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	// Create fake clientset that will return error on exec
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "jump-pod",
				Namespace: "test-ns",
			},
		}, nil
	})

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	client, server := net.Pipe()
	defer server.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		client.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// This should fail because restConfig is nil
	err := handler.HandleConnection(ctx, server, 5432)
	if err == nil {
		t.Error("Expected error with nil restConfig, got nil")
	}
}

func TestJumpHandler_ensureJumpPodExists_NoCreateConfig(t *testing.T) {
	// When no create config is set, ensureJumpPodExists should be a no-op
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "my-jump-pod",
			// No Create config
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	clientset := fake.NewSimpleClientset()
	handler := NewJumpHandler(route, nil, clientset, nil, false)

	err := handler.ensureJumpPodExists(context.Background())
	if err != nil {
		t.Errorf("ensureJumpPodExists should be no-op without create config, got error: %v", err)
	}
}

func TestJumpHandler_ensureJumpPodExists_PodAlreadyExists(t *testing.T) {
	// When pod already exists, ensureJumpPodExists should be a no-op
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	clientset := fake.NewSimpleClientset(existingPod)

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
			Create: &config.CreateConfig{
				Image: "alpine:3.19",
			},
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, true)

	err := handler.ensureJumpPodExists(context.Background())
	if err != nil {
		t.Errorf("ensureJumpPodExists should succeed when pod exists, got error: %v", err)
	}
}

func TestJumpHandler_ensureJumpPodExists_CreatesPod(t *testing.T) {
	// Create fake clientset with no existing pod
	clientset := fake.NewSimpleClientset()

	// Track if pod was created and store the created pod
	podCreated := false
	var createdPod *corev1.Pod

	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		podCreated = true
		createAction := action.(k8stesting.CreateAction)
		pod := createAction.GetObject().(*corev1.Pod).DeepCopy()
		// Set the pod as running and ready
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		}
		createdPod = pod
		return false, nil, nil // Let the default handler store it
	})

	// Return ready status when getting the pod
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() == "autotunnel-jump" && createdPod != nil {
			return true, createdPod, nil
		}
		return false, nil, nil
	})

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
			Create: &config.CreateConfig{
				Image: "alpine:3.19",
			},
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	err := handler.ensureJumpPodExists(context.Background())
	if err != nil {
		t.Errorf("ensureJumpPodExists failed: %v", err)
	}

	if !podCreated {
		t.Error("expected pod to be created")
	}
}

func TestJumpHandler_buildJumpPodSpec(t *testing.T) {
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
			Create: &config.CreateConfig{
				Image: "alpine:3.19",
			},
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, nil, nil, false)
	pod := handler.buildJumpPodSpec()

	// Verify pod name and namespace
	if pod.Name != "autotunnel-jump" {
		t.Errorf("expected pod name 'autotunnel-jump', got %q", pod.Name)
	}
	if pod.Namespace != "test-ns" {
		t.Errorf("expected namespace 'test-ns', got %q", pod.Namespace)
	}

	// Verify labels
	expectedLabels := map[string]string{
		"app.kubernetes.io/name":       "autotunnel-jump",
		"app.kubernetes.io/managed-by": "autotunnel",
	}
	for key, expectedValue := range expectedLabels {
		if pod.Labels[key] != expectedValue {
			t.Errorf("expected label %q=%q, got %q", key, expectedValue, pod.Labels[key])
		}
	}

	// Verify container
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	container := pod.Spec.Containers[0]
	if container.Name != "jump" {
		t.Errorf("expected container name 'jump', got %q", container.Name)
	}
	if container.Image != "alpine:3.19" {
		t.Errorf("expected image 'alpine:3.19', got %q", container.Image)
	}

	// Verify command
	expectedCommand := []string{"sleep", "infinity"}
	if len(container.Command) != len(expectedCommand) {
		t.Errorf("expected command %v, got %v", expectedCommand, container.Command)
	} else {
		for i, cmd := range expectedCommand {
			if container.Command[i] != cmd {
				t.Errorf("expected command[%d]=%q, got %q", i, cmd, container.Command[i])
			}
		}
	}

	// Verify resources are set
	if container.Resources.Requests == nil {
		t.Error("expected resource requests to be set")
	}
	if container.Resources.Limits == nil {
		t.Error("expected resource limits to be set")
	}
}

func TestJumpHandler_buildJumpPodSpec_CustomCommand(t *testing.T) {
	customCommand := []string{"tail", "-f", "/dev/null"}
	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
			Create: &config.CreateConfig{
				Image:   "alpine:3.19",
				Command: customCommand,
			},
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, nil, nil, false)
	pod := handler.buildJumpPodSpec()

	// Verify custom command is used
	container := pod.Spec.Containers[0]
	if len(container.Command) != len(customCommand) {
		t.Fatalf("expected command length %d, got %d", len(customCommand), len(container.Command))
	}
	for i, cmd := range customCommand {
		if container.Command[i] != cmd {
			t.Errorf("expected command[%d]=%q, got %q", i, cmd, container.Command[i])
		}
	}
}

func TestJumpHandler_waitForPodReady_Timeout(t *testing.T) {
	// Create a pod that never becomes ready
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pod)

	route := config.JumpRouteConfig{
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	// Use a short timeout for testing
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := handler.waitForPodReady(ctx, "autotunnel-jump")
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestJumpHandler_waitForPodReady_PodFailed(t *testing.T) {
	// Create a pod that fails
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}

	clientset := fake.NewSimpleClientset(pod)

	route := config.JumpRouteConfig{
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	err := handler.waitForPodReady(context.Background(), "autotunnel-jump")
	if err == nil {
		t.Error("expected error for failed pod, got nil")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("expected error about pod failure, got: %v", err)
	}
}

func TestJumpHandler_discoverJumpPod_WithCreate(t *testing.T) {
	// Test that discoverJumpPod calls ensureJumpPodExists
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	clientset := fake.NewSimpleClientset(existingPod)

	route := config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod: "autotunnel-jump",
			Create: &config.CreateConfig{
				Image: "alpine:3.19",
			},
		},
		Target: config.TargetConfig{
			Host: "database.internal",
			Port: 5432,
		},
	}

	handler := NewJumpHandler(route, nil, clientset, nil, false)

	podName, containerName, err := handler.discoverJumpPod(context.Background())
	if err != nil {
		t.Fatalf("discoverJumpPod failed: %v", err)
	}

	if podName != "autotunnel-jump" {
		t.Errorf("expected pod name 'autotunnel-jump', got %q", podName)
	}
	if containerName != "" {
		t.Errorf("expected empty container name, got %q", containerName)
	}
}
