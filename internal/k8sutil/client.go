package k8sutil

import (
	"fmt"
	"log"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientFactory creates and caches Kubernetes clients per context
type ClientFactory struct {
	clients   map[string]*cachedClient
	clientsMu sync.RWMutex
	verbose   bool
}

type cachedClient struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
}

// NewClientFactory creates a new client factory
func NewClientFactory(verbose bool) *ClientFactory {
	return &ClientFactory{
		clients: make(map[string]*cachedClient),
		verbose: verbose,
	}
}

// GetClientForContext returns a cached or new clientset for the given context.
// kubeconfigPaths can specify multiple kubeconfig files to merge (like KUBECONFIG=a:b:c).
func (f *ClientFactory) GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	// Fast path: check cache
	f.clientsMu.RLock()
	if client, ok := f.clients[contextName]; ok {
		f.clientsMu.RUnlock()
		return client.clientset, client.restConfig, nil
	}
	f.clientsMu.RUnlock()

	// Slow path: create client
	f.clientsMu.Lock()
	defer f.clientsMu.Unlock()

	// Double-check after acquiring write lock
	if client, ok := f.clients[contextName]; ok {
		return client.clientset, client.restConfig, nil
	}

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

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create clientset for context %s: %w", contextName, err)
	}

	f.clients[contextName] = &cachedClient{
		clientset:  clientset,
		restConfig: restConfig,
	}

	if f.verbose {
		log.Printf("Created k8s client for context: %s", contextName)
	}

	return clientset, restConfig, nil
}

// Clear clears all cached clients (for shutdown)
func (f *ClientFactory) Clear() {
	f.clientsMu.Lock()
	f.clients = make(map[string]*cachedClient)
	f.clientsMu.Unlock()
}

// InjectClient adds a pre-configured client for a context (for testing)
func (f *ClientFactory) InjectClient(contextName string, clientset *kubernetes.Clientset, restConfig *rest.Config) {
	f.clientsMu.Lock()
	f.clients[contextName] = &cachedClient{
		clientset:  clientset,
		restConfig: restConfig,
	}
	f.clientsMu.Unlock()
}
