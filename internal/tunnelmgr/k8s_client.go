package tunnelmgr

import (
	"fmt"
	"log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// k8sClient holds a cached Kubernetes clientset and REST config for a context
type k8sClient struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
}

// getClientsetAndConfig returns a cached or newly created k8s client for the given context
func (m *Manager) getClientsetAndConfig(kubeconfig, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	// Try read lock first for cached client
	m.k8sClientsMu.RLock()
	if client, ok := m.k8sClients[contextName]; ok {
		m.k8sClientsMu.RUnlock()
		return client.clientset, client.restConfig, nil
	}
	m.k8sClientsMu.RUnlock()

	// Acquire write lock to create new client
	m.k8sClientsMu.Lock()
	defer m.k8sClientsMu.Unlock()

	// Double-check after acquiring write lock
	if client, ok := m.k8sClients[contextName]; ok {
		return client.clientset, client.restConfig, nil
	}

	// Build REST config
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfig
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

	// Cache and return
	m.k8sClients[contextName] = &k8sClient{
		clientset:  clientset,
		restConfig: restConfig,
	}

	if m.config.Verbose {
		log.Printf("Created k8s client for context: %s", contextName)
	}

	return clientset, restConfig, nil
}
