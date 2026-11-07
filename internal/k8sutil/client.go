package k8sutil

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientFactory creates and caches Kubernetes clients per kubeconfig list and context
type ClientFactory struct {
	clients   map[string]*cachedClient
	clientsMu sync.RWMutex
	verbose   bool
}

type cachedClient struct {
	clientset   *kubernetes.Clientset
	restConfig  *rest.Config
	fingerprint string // state of the kubeconfig files this client was built from
}

// NewClientFactory creates a new client factory
func NewClientFactory(verbose bool) *ClientFactory {
	return &ClientFactory{
		clients: make(map[string]*cachedClient),
		verbose: verbose,
	}
}

func newLoadingRules(kubeconfigPaths []string) *clientcmd.ClientConfigLoadingRules {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(kubeconfigPaths) > 0 {
		loadingRules.Precedence = kubeconfigPaths
	}
	return loadingRules
}

// The same context name in two kubeconfigs can be two different clusters
func clientKey(paths []string, contextName string) string {
	return strings.Join(paths, "\x00") + "\x00" + contextName
}

// kubeconfigFingerprint changes when any of the files is rewritten, created or removed.
func kubeconfigFingerprint(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "%d:%d;", info.Size(), info.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	return b.String()
}

// GetClientForContext returns a cached or new clientset for the given context.
// kubeconfigPaths can specify multiple kubeconfig files to merge (like KUBECONFIG=a:b:c).
// A client is rebuilt when those files change, such as after recreating a cluster.
// Running tunnels keep the client they started with.
func (f *ClientFactory) GetClientForContext(kubeconfigPaths []string, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	loadingRules := newLoadingRules(kubeconfigPaths)
	paths := loadingRules.GetLoadingPrecedence()
	key := clientKey(paths, contextName)
	// Taken before loading: a write during the load only causes one more rebuild next time
	fingerprint := kubeconfigFingerprint(paths)

	// Fast path: check cache
	f.clientsMu.RLock()
	if client, ok := f.clients[key]; ok && client.fingerprint == fingerprint {
		f.clientsMu.RUnlock()
		return client.clientset, client.restConfig, nil
	}
	f.clientsMu.RUnlock()

	// Slow path: create client
	f.clientsMu.Lock()
	defer f.clientsMu.Unlock()

	// Double-check after acquiring write lock
	if client, ok := f.clients[key]; ok && client.fingerprint == fingerprint {
		return client.clientset, client.restConfig, nil
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

	f.clients[key] = &cachedClient{
		clientset:   clientset,
		restConfig:  restConfig,
		fingerprint: fingerprint,
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

// InjectClient adds a pre-configured client for a kubeconfig list and context (for testing)
func (f *ClientFactory) InjectClient(kubeconfigPaths []string, contextName string, clientset *kubernetes.Clientset, restConfig *rest.Config) {
	paths := newLoadingRules(kubeconfigPaths).GetLoadingPrecedence()
	f.clientsMu.Lock()
	f.clients[clientKey(paths, contextName)] = &cachedClient{
		clientset:   clientset,
		restConfig:  restConfig,
		fingerprint: kubeconfigFingerprint(paths),
	}
	f.clientsMu.Unlock()
}
