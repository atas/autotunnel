package k8sutil

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// FindReadyPod finds a ready pod matching the given selector labels.
// If no pods are found or none are ready, it returns an error or falls back
// to the first running pod (which might still work even if not ready).
//
// Parameters:
//   - ctx: context for the API call
//   - clientset: Kubernetes clientset
//   - namespace: the namespace to search in
//   - selectorLabels: label selector to match pods
//   - serviceName: service name for error messages (optional, can be empty)
func FindReadyPod(ctx context.Context, clientset kubernetes.Interface, namespace string, selectorLabels map[string]string, serviceName string) (*corev1.Pod, error) {
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: selectorLabels,
	})

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		if serviceName != "" {
			return nil, fmt.Errorf("no running pods found for service %s", serviceName)
		}
		return nil, fmt.Errorf("no running pods found matching selector %s", selector)
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

	// Not ideal but better than failing - pod might still work
	return &pods.Items[0], nil
}

// WaitForPodReady polls until a pod is ready or timeout/context cancellation
func WaitForPodReady(ctx context.Context, clientset kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	const pollInterval = 1 * time.Second
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s to be ready", name)
		}

		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("pod %s was deleted", name)
			}
			// Transient error, continue polling
		} else {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}
			if pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod %s failed", name)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
