package tunnelmgr

import (
	"fmt"
	"log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type k8sClient struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
}

func (m *Manager) getClientsetAndConfig(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	// fast path: already have a client for this context
	m.k8sClientsMu.RLock()
	if client, ok := m.k8sClients[contextName]; ok {
		m.k8sClientsMu.RUnlock()
		return client.clientset, client.restConfig, nil
	}
	m.k8sClientsMu.RUnlock()

	m.k8sClientsMu.Lock()
	defer m.k8sClientsMu.Unlock()

	// another goroutine might have created it while we were waiting for the lock
	if client, ok := m.k8sClients[contextName]; ok {
		return client.clientset, client.restConfig, nil
	}

	// client-go can merge multiple kubeconfig files (like kubectl does with KUBECONFIG=a:b:c)
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(kubeconfigPaths) > 0 {
		loadingRules.Precedence = kubeconfigPaths
	}
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build REST config for context %s: %w", contextName, err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create clientset for context %s: %w", contextName, err)
	}

	m.k8sClients[contextName] = &k8sClient{
		clientset:  clientset,
		restConfig: restConfig,
	}

	if m.config.Verbose {
		log.Printf("Created k8s client for context: %s", contextName)
	}

	return clientset, restConfig, nil
}
