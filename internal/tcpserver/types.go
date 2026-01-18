package tcpserver

import (
	"github.com/atas/autotunnel/internal/tunnelmgr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Manager interface {
	GetOrCreateTCPTunnel(localPort int) (tunnelmgr.TunnelHandle, error)
	GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error)
}
