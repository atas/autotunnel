package k8sutil

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GetService fetches a Kubernetes service by name
func GetService(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (*corev1.Service, error) {
	return clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ResolveServicePort finds the target port for a given service port.
// Returns the container port number and port name (if named port).
// If port name is returned, caller must resolve it against pod spec.
func ResolveServicePort(svc *corev1.Service, port int) (targetPort int, portName string) {
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == port {
			if p.TargetPort.IntVal != 0 {
				return int(p.TargetPort.IntVal), ""
			}
			return port, p.TargetPort.StrVal
		}
	}
	return port, ""
}

// ResolveNamedPort finds a container port by name in a pod spec
func ResolveNamedPort(pod *corev1.Pod, portName string) (int, error) {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == portName {
				return int(port.ContainerPort), nil
			}
		}
	}
	return 0, fmt.Errorf("could not resolve named port %q in pod %s", portName, pod.Name)
}
