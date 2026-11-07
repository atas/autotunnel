package tcpserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/k8sutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/streaming/pkg/httpstream"
	spdystream "k8s.io/streaming/pkg/httpstream/spdy"
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
			wantSocat:  "socat -t 30 'STDIN!!STDOUT,shut-close' 'TCP:database.internal:5432'",
			wantNc:     "nc 'database.internal' 5432",
		},
		{
			name:       "localhost target",
			targetHost: "127.0.0.1",
			targetPort: 3306,
			wantSocat:  "socat -t 30 'STDIN!!STDOUT,shut-close' 'TCP:127.0.0.1:3306'",
			wantNc:     "nc '127.0.0.1' 3306",
		},
		{
			name:       "RDS endpoint",
			targetHost: "mydb.cluster-xyz.us-east-1.rds.amazonaws.com",
			targetPort: 5432,
			wantSocat:  "socat -t 30 'STDIN!!STDOUT,shut-close' 'TCP:mydb.cluster-xyz.us-east-1.rds.amazonaws.com:5432'",
			wantNc:     "nc 'mydb.cluster-xyz.us-east-1.rds.amazonaws.com' 5432",
		},
		{
			name:       "IPv6 address",
			targetHost: "2001:db8::1",
			targetPort: 5432,
			wantSocat:  "socat -t 30 'STDIN!!STDOUT,shut-close' 'TCP:[2001:db8::1]:5432'",
			wantNc:     "nc '[2001:db8::1]' 5432",
		},
		{
			name:       "IPv6 loopback",
			targetHost: "::1",
			targetPort: 3306,
			wantSocat:  "socat -t 30 'STDIN!!STDOUT,shut-close' 'TCP:[::1]:3306'",
			wantNc:     "nc '[::1]' 3306",
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

			expectedCmd := "if command -v socat >/dev/null 2>&1; then exec " + tt.wantSocat + "; else exec " + tt.wantNc + "; fi"
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
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{
		conn:   server,
		ctx:    ctx,
		cancel: cancel,
	}

	// An expired deadline is a read error other than EOF
	_ = server.SetReadDeadline(time.Now())

	buf := make([]byte, 10)
	_, err := crw.Read(buf)

	if err == nil {
		t.Error("Expected error on read past the deadline, got nil")
	}

	// Context should be cancelled after read error
	select {
	case <-ctx.Done():
		// Good - context was cancelled
	default:
		t.Error("Context should be cancelled after read error")
	}
}

func TestConnReadWriter_WriteErrorCancelsContext(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{conn: server, ctx: ctx, cancel: cancel}

	if _, err := crw.Write([]byte("x")); err == nil {
		t.Fatal("Expected error writing to a closed client, got nil")
	}
	if ctx.Err() == nil {
		t.Error("Context should be cancelled once the client cannot be written to")
	}
}

func TestConnReadWriter_EOFWaitsForGrace(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crw := &connReadWriter{conn: server, ctx: ctx, cancel: cancel, graceDur: 200 * time.Millisecond}

	if _, err := crw.Read(make([]byte, 10)); err != io.EOF {
		t.Fatalf("Read error = %v, want io.EOF", err)
	}
	if ctx.Err() != nil {
		t.Fatal("EOF may be a half-close, it must not cancel straight away")
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Grace period never ended the connection")
	}
}

func TestConnReadWriter_WritesExtendGrace(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { _, _ = io.Copy(io.Discard, client) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	grace := 500 * time.Millisecond
	crw := &connReadWriter{conn: server, ctx: ctx, cancel: cancel, graceDur: grace}
	crw.startGrace()

	// A response that keeps flowing outlives the grace period
	for start := time.Now(); time.Since(start) < 2*grace; time.Sleep(grace / 10) {
		if _, err := crw.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("Connection ended while data was still flowing")
		}
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Grace period never ended the connection once data stopped")
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
	// When a ready pod already exists, ensureJumpPodExists should do nothing to it
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
		},
		Status: readyPodStatus(),
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

func TestWaitForPodReady_Timeout(t *testing.T) {
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

	// Use a short timeout for testing
	err := k8sutil.WaitForPodReady(context.Background(), clientset, "test-ns", "autotunnel-jump", 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestWaitForPodReady_PodFailed(t *testing.T) {
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

	err := k8sutil.WaitForPodReady(context.Background(), clientset, "test-ns", "autotunnel-jump", 5*time.Second)
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

func readyPodStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
}

func createRoute(timeout time.Duration) config.JumpRouteConfig {
	return config.JumpRouteConfig{
		Context:   "test-context",
		Namespace: "test-ns",
		Via: config.ViaConfig{
			Pod:    "autotunnel-jump",
			Create: &config.CreateConfig{Image: "alpine:3.19", Timeout: timeout},
		},
		Target: config.TargetConfig{Host: "database.internal", Port: 5432},
	}
}

func managedPod(status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autotunnel-jump",
			Namespace: "test-ns",
			UID:       types.UID("old-uid"),
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "autotunnel"},
		},
		Status: status,
	}
}

// readyOnCreate makes every pod created through clientset start out ready.
func readyOnCreate(clientset *fake.Clientset) {
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Status = readyPodStatus()
		return false, nil, nil
	})
}

func countActions(clientset *fake.Clientset, verb string) int {
	n := 0
	for _, a := range clientset.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func setPodReady(t *testing.T, clientset *fake.Clientset) {
	t.Helper()
	pods := clientset.CoreV1().Pods("test-ns")
	pod, err := pods.Get(context.Background(), "autotunnel-jump", metav1.GetOptions{})
	if err != nil {
		t.Errorf("get pod: %v", err)
		return
	}
	pod.Status = readyPodStatus()
	if _, err := pods.UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Errorf("update pod status: %v", err)
	}
}

func TestJumpHandler_ensureJumpPodExists_WaitsForPendingPod(t *testing.T) {
	clientset := fake.NewSimpleClientset(managedPod(corev1.PodStatus{Phase: corev1.PodPending}))
	handler := NewJumpHandler(createRoute(10*time.Second), nil, clientset, nil, false)

	var ready atomic.Bool
	go func() {
		time.Sleep(200 * time.Millisecond)
		ready.Store(true)
		setPodReady(t, clientset)
	}()

	if err := handler.ensureJumpPodExists(context.Background()); err != nil {
		t.Fatalf("ensureJumpPodExists: %v", err)
	}
	if !ready.Load() {
		t.Error("returned before the pending pod became ready")
	}
	if n := countActions(clientset, "delete") + countActions(clientset, "create"); n != 0 {
		t.Errorf("a healthy pending pod must be left alone, got %d create/delete calls", n)
	}
}

func TestJumpHandler_ensureJumpPodExists_TimesOutOnPendingPod(t *testing.T) {
	clientset := fake.NewSimpleClientset(managedPod(corev1.PodStatus{Phase: corev1.PodPending}))
	handler := NewJumpHandler(createRoute(100*time.Millisecond), nil, clientset, nil, false)

	if err := handler.ensureJumpPodExists(context.Background()); err == nil {
		t.Fatal("expected a timeout for a pod that never becomes ready")
	}
}

func TestJumpHandler_ensureJumpPodExists_RecreatesFinishedPod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			clientset := fake.NewSimpleClientset(managedPod(corev1.PodStatus{Phase: phase}))
			readyOnCreate(clientset)
			handler := NewJumpHandler(createRoute(10*time.Second), nil, clientset, nil, false)

			if err := handler.ensureJumpPodExists(context.Background()); err != nil {
				t.Fatalf("ensureJumpPodExists: %v", err)
			}

			pod, err := clientset.CoreV1().Pods("test-ns").Get(context.Background(), "autotunnel-jump", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get pod: %v", err)
			}
			if pod.UID == "old-uid" || pod.Status.Phase != corev1.PodRunning {
				t.Errorf("pod was not replaced: uid=%q phase=%q", pod.UID, pod.Status.Phase)
			}

			// A connection racing this one must not be able to delete the replacement.
			deletes := 0
			for _, a := range clientset.Actions() {
				if del, ok := a.(k8stesting.DeleteAction); ok {
					deletes++
					pre := del.GetDeleteOptions().Preconditions
					if pre == nil || pre.UID == nil || *pre.UID != "old-uid" {
						t.Errorf("delete has no UID precondition for the dead pod: %+v", pre)
					}
				}
			}
			if deletes != 1 {
				t.Errorf("expected one delete call, got %d", deletes)
			}
		})
	}
}

func TestJumpHandler_ensureJumpPodExists_KeepsPodItDidNotCreate(t *testing.T) {
	pod := managedPod(corev1.PodStatus{Phase: corev1.PodFailed})
	pod.Labels = nil
	clientset := fake.NewSimpleClientset(pod)
	handler := NewJumpHandler(createRoute(10*time.Second), nil, clientset, nil, false)

	err := handler.ensureJumpPodExists(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not created by autotunnel") {
		t.Fatalf("expected an error naming the foreign pod, got %v", err)
	}
	if n := countActions(clientset, "delete"); n != 0 {
		t.Errorf("deleted a pod autotunnel did not create (%d delete calls)", n)
	}
}

func TestJumpHandler_ensureJumpPodExists_WaitsOutTerminatingPod(t *testing.T) {
	now := metav1.Now()
	// Ready=True on purpose: a pod being deleted can still report ready.
	pod := managedPod(readyPodStatus())
	pod.DeletionTimestamp = &now
	clientset := fake.NewSimpleClientset(pod)
	readyOnCreate(clientset)
	handler := NewJumpHandler(createRoute(10*time.Second), nil, clientset, nil, false)

	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "test-ns", "autotunnel-jump"); err != nil {
			t.Errorf("remove terminating pod: %v", err)
		}
	}()

	if err := handler.ensureJumpPodExists(context.Background()); err != nil {
		t.Fatalf("ensureJumpPodExists: %v", err)
	}
	if n := countActions(clientset, "create"); n != 1 {
		t.Errorf("expected the pod to be recreated once it was gone, got %d create calls", n)
	}
	if n := countActions(clientset, "delete"); n != 0 {
		t.Errorf("a terminating pod is already on its way out, got %d delete calls", n)
	}
}

func TestJumpHandler_ensureJumpPodExists_LostDeleteRaceIsNotFatal(t *testing.T) {
	clientset := fake.NewSimpleClientset(managedPod(corev1.PodStatus{Phase: corev1.PodFailed}))
	// Another connection replaced the dead pod between our Get and our Delete.
	clientset.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		fresh := managedPod(readyPodStatus())
		fresh.UID = "fresh-uid"
		if err := clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), fresh, "test-ns"); err != nil {
			t.Errorf("replace pod: %v", err)
		}
		return true, nil, errors.NewConflict(schema.GroupResource{Resource: "pods"}, "autotunnel-jump", fmt.Errorf("uid mismatch"))
	})
	handler := NewJumpHandler(createRoute(10*time.Second), nil, clientset, nil, false)

	if err := handler.ensureJumpPodExists(context.Background()); err != nil {
		t.Fatalf("ensureJumpPodExists: %v", err)
	}
}

func TestJumpHandler_ensureJumpPodExists_ConcurrentFirstConnections(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	route := createRoute(10 * time.Second)

	var ready atomic.Bool
	go func() {
		time.Sleep(200 * time.Millisecond)
		ready.Store(true)
		setPodReady(t, clientset)
	}()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// server.go builds one handler per connection, so these share nothing.
			handler := NewJumpHandler(route, nil, clientset, nil, false)
			if err := handler.ensureJumpPodExists(context.Background()); err != nil {
				t.Errorf("ensureJumpPodExists: %v", err)
			}
			if !ready.Load() {
				t.Error("a connection went on before the pod was ready")
			}
		}()
	}
	wg.Wait()

	if n := countActions(clientset, "delete"); n != 0 {
		t.Errorf("no pod should be deleted, got %d delete calls", n)
	}
}

// runForwardCommand runs the command in a real shell with only the given fake
// tools on PATH, and returns one log line per tool run: "<tool> <pid> <args>".
func runForwardCommand(t *testing.T, cmd string, tools map[string]int) (log []string, shellPid int, err error) {
	t.Helper()
	shell, lookErr := exec.LookPath("sh")
	if lookErr != nil {
		t.Skip("no sh on this machine")
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	for tool, exit := range tools {
		script := fmt.Sprintf("#!/bin/sh\necho \"%s $$ $*\" >> %q\nexit %d\n", tool, logFile, exit)
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := exec.Command(shell, "-c", cmd)
	c.Env = []string{"PATH=" + dir}
	err = c.Run()

	data, readErr := os.ReadFile(logFile)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(data) > 0 {
		log = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return log, c.Process.Pid, err
}

func forwardCommandFor(t *testing.T, host string, port int) string {
	t.Helper()
	handler := NewJumpHandler(config.JumpRouteConfig{Target: config.TargetConfig{Host: host, Port: port}}, nil, nil, nil, false)
	cmd, err := handler.buildForwardCommand()
	if err != nil {
		t.Fatalf("buildForwardCommand: %v", err)
	}
	return cmd
}

func TestForwardCommand_FailedSocatDoesNotFallBackToNc(t *testing.T) {
	log, _, err := runForwardCommand(t, forwardCommandFor(t, "db.internal", 6379), map[string]int{"socat": 1, "nc": 0})
	if err == nil {
		t.Error("the failure of socat must reach the caller")
	}
	if len(log) != 1 || !strings.HasPrefix(log[0], "socat ") {
		t.Errorf("only socat should run, ran: %q", log)
	}
}

func TestForwardCommand_ReplacesShellWithSocat(t *testing.T) {
	log, shellPid, err := runForwardCommand(t, forwardCommandFor(t, "db.internal", 6379), map[string]int{"socat": 0, "nc": 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := fmt.Sprintf("socat %d -t 30 STDIN!!STDOUT,shut-close TCP:db.internal:6379", shellPid)
	if len(log) != 1 || log[0] != want {
		t.Errorf("ran %q, want [%q]", log, want)
	}
}

func TestForwardCommand_UsesNcWhenSocatIsMissing(t *testing.T) {
	log, shellPid, err := runForwardCommand(t, forwardCommandFor(t, "db.internal", 6379), map[string]int{"nc": 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := fmt.Sprintf("nc %d db.internal 6379", shellPid)
	if len(log) != 1 || log[0] != want {
		t.Errorf("ran %q, want [%q]", log, want)
	}
}

func TestShellQuote(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}
	for _, value := range []string{"db.internal", "[::1]", "a b", "it's", "$(touch x); `y` \\ *"} {
		out, err := exec.Command(shell, "-c", "printf %s "+shellQuote(value)).Output()
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if string(out) != value {
			t.Errorf("shell saw %q, want %q", out, value)
		}
	}
}

func TestBuildForwardCommand_RejectsInvalidHost(t *testing.T) {
	handler := NewJumpHandler(config.JumpRouteConfig{Target: config.TargetConfig{Host: "db; rm -rf /", Port: 1}}, nil, nil, nil, false)
	if cmd, err := handler.buildForwardCommand(); err == nil {
		t.Errorf("expected an error, got command %q", cmd)
	}
}

// fakeExecProcess plays the remote process behind the exec API.
type fakeExecProcess func(stdin io.Reader, stdout io.WriteCloser)

// fakeExecHandler returns a handler whose exec calls go over real SPDY through
// real client-go to process, so client-go's stream handling is part of the test.
func fakeExecHandler(t *testing.T, process fakeExecProcess) *JumpHandler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := httpstream.Handshake(r, w, []string{"v4.channel.k8s.io"}); err != nil {
			return
		}
		streams := make(chan httpstream.Stream, 4)
		conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
			streams <- s
			return nil
		})
		if conn == nil {
			return
		}
		defer func() { _ = conn.Close() }()

		byType := map[string]httpstream.Stream{}
		for len(byType) < 4 {
			select {
			case s := <-streams:
				byType[s.Headers().Get(corev1.StreamType)] = s
			case <-time.After(5 * time.Second):
				return
			}
		}

		process(byType[corev1.StreamTypeStdin], byType[corev1.StreamTypeStdout])

		byType[corev1.StreamTypeStdout].Close()
		byType[corev1.StreamTypeStderr].Close()
		_, _ = io.WriteString(byType[corev1.StreamTypeError], `{"status":"Success"}`)
		byType[corev1.StreamTypeError].Close()
		<-conn.CloseChan()
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(srv.CloseClientConnections)

	restConfig := &rest.Config{Host: srv.URL}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	route := config.JumpRouteConfig{
		Namespace: "test-ns",
		Via:       config.ViaConfig{Pod: "jump"},
		Target:    config.TargetConfig{Host: "db.internal", Port: 6379},
	}
	return NewJumpHandler(route, nil, clientset, restConfig, false)
}

func tcpConnPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

func TestJumpHandler_HandleConnection_ClientHalfCloseKeepsResponse(t *testing.T) {
	handler := fakeExecHandler(t, func(stdin io.Reader, stdout io.WriteCloser) {
		request, _ := io.ReadAll(stdin) // only returns once stdin was half-closed
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprintf(stdout, "RESP-to-%s", request)
	})
	client, server := tcpConnPair(t)

	done := make(chan error, 1)
	go func() {
		done <- handler.HandleConnection(context.Background(), server, 6379)
		_ = server.Close()
	}()

	if _, err := client.Write([]byte("PING")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != "RESP-to-PING" {
		t.Errorf("got response %q", got)
	}
	if err := <-done; err != nil {
		t.Errorf("HandleConnection: %v", err)
	}
}

// The target hanging up must reach the client at once, even though socat stays
// alive for its -t period waiting for stdin.
func TestJumpHandler_HandleConnection_TargetCloseReachesClient(t *testing.T) {
	stdinClosed := make(chan struct{})
	handler := fakeExecHandler(t, func(stdin io.Reader, stdout io.WriteCloser) {
		_, _ = io.WriteString(stdout, "BYE")
		stdout.Close()
		_, _ = io.Copy(io.Discard, stdin)
		close(stdinClosed)
	})
	client, server := tcpConnPair(t)

	done := make(chan error, 1)
	go func() {
		done <- handler.HandleConnection(context.Background(), server, 6379)
		_ = server.Close()
	}()

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client should see EOF while its side is still open: %v", err)
	}
	if string(got) != "BYE" {
		t.Errorf("got %q", got)
	}

	// The client closing in turn ends the remote process and the handler.
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stdinClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("client close did not reach the remote stdin")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("HandleConnection: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HandleConnection did not return")
	}
}
