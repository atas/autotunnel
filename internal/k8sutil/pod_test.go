package k8sutil

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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

	pod, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err != nil {
		t.Fatalf("FindReadyPod() error = %v", err)
	}
	if pod.Name != "pod-ready" {
		t.Errorf("FindReadyPod() selected %q, want %q", pod.Name, "pod-ready")
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

	pod, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err != nil {
		t.Fatalf("FindReadyPod() error = %v", err)
	}
	if pod.Name != "pod-running" {
		t.Errorf("FindReadyPod() selected %q, want %q", pod.Name, "pod-running")
	}
}

func TestFindReadyPod_NoPods(t *testing.T) {
	ctx := context.Background()

	fakeClient := fake.NewSimpleClientset()

	_, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err == nil {
		t.Fatal("FindReadyPod() expected error for no pods, got nil")
	}
}

func TestFindReadyPod_NoPodsWithoutServiceName(t *testing.T) {
	ctx := context.Background()

	fakeClient := fake.NewSimpleClientset()

	_, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "")
	if err == nil {
		t.Fatal("FindReadyPod() expected error for no pods, got nil")
	}
	// Should use selector in error message when no service name provided
	if err.Error() == "no running pods found for service " {
		t.Error("Error message should not mention empty service name")
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

	_, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err == nil {
		t.Fatal("FindReadyPod() expected error when no matching pods, got nil")
	}
}

func TestFindReadyPod_IgnoresDifferentNamespace(t *testing.T) {
	ctx := context.Background()

	// Create a pod in a different namespace
	podInOtherNs := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-other-ns",
			Namespace: "other-ns",
			Labels:    map[string]string{"app": "test"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(podInOtherNs)

	_, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err == nil {
		t.Fatal("FindReadyPod() expected error when pod is in different namespace, got nil")
	}
}

func TestFindReadyPod_MultipleReadyPods(t *testing.T) {
	ctx := context.Background()

	// Create multiple ready pods
	readyPod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-ready-1",
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

	readyPod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-ready-2",
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

	fakeClient := fake.NewSimpleClientset(readyPod1, readyPod2)

	pod, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test"}, "test-svc")
	if err != nil {
		t.Fatalf("FindReadyPod() error = %v", err)
	}
	// Should return one of the ready pods (order not guaranteed)
	if pod.Name != "pod-ready-1" && pod.Name != "pod-ready-2" {
		t.Errorf("FindReadyPod() selected %q, want one of the ready pods", pod.Name)
	}
}

func TestFindReadyPod_MultipleLabels(t *testing.T) {
	ctx := context.Background()

	// Create a pod with multiple labels
	multiLabelPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-multi-label",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "test", "version": "v1", "tier": "backend"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(multiLabelPod)

	// Should find pod with subset of labels
	pod, err := FindReadyPod(ctx, fakeClient, "test-ns", map[string]string{"app": "test", "version": "v1"}, "test-svc")
	if err != nil {
		t.Fatalf("FindReadyPod() error = %v", err)
	}
	if pod.Name != "pod-multi-label" {
		t.Errorf("FindReadyPod() selected %q, want %q", pod.Name, "pod-multi-label")
	}
}
