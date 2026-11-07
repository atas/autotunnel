package k8sutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKubeconfig(t *testing.T, path, contextName, server string) {
	t.Helper()
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[2]s
  cluster:
    server: %[1]s
contexts:
- name: %[2]s
  context:
    cluster: %[2]s
    user: %[2]s
users:
- name: %[2]s
  user:
    token: t
current-context: %[2]s
`, server, contextName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGetClientForContext_SameContextNameInDifferentKubeconfigs(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "dev")
	prod := filepath.Join(dir, "prod")
	writeKubeconfig(t, dev, "default", "https://dev.example:6443")
	writeKubeconfig(t, prod, "default", "https://prod.example:6443")

	f := NewClientFactory(false)

	_, devCfg, err := f.GetClientForContext([]string{dev}, "default")
	if err != nil {
		t.Fatal(err)
	}
	_, prodCfg, err := f.GetClientForContext([]string{prod}, "default")
	if err != nil {
		t.Fatal(err)
	}

	if devCfg.Host != "https://dev.example:6443" {
		t.Errorf("dev host = %s", devCfg.Host)
	}
	if prodCfg.Host != "https://prod.example:6443" {
		t.Errorf("prod host = %s, want the prod cluster, not the one cached first", prodCfg.Host)
	}
}

func TestGetClientForContext_CachedWhileKubeconfigUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, "ctx", "https://a.example:6443")

	f := NewClientFactory(false)

	first, _, err := f.GetClientForContext([]string{path}, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.GetClientForContext([]string{path}, "ctx")
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Error("expected the cached client when the kubeconfig has not changed")
	}
}

func TestGetClientForContext_RebuildsWhenKubeconfigChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, "ctx", "https://127.0.0.1:50001")

	f := NewClientFactory(false)

	if _, _, err := f.GetClientForContext([]string{path}, "ctx"); err != nil {
		t.Fatal(err)
	}

	// Same size, as when a recreated cluster gets a new port: only the mtime differs.
	writeKubeconfig(t, path, "ctx", "https://127.0.0.1:50002")
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	_, cfg, err := f.GetClientForContext([]string{path}, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://127.0.0.1:50002" {
		t.Errorf("host = %s, want the rewritten server", cfg.Host)
	}
}

func TestGetClientForContext_RebuildsWhenAnyMergedKubeconfigChanges(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	writeKubeconfig(t, first, "other", "https://other.example:6443")
	writeKubeconfig(t, second, "ctx", "https://old.example:6443")
	paths := []string{first, second}

	f := NewClientFactory(false)

	if _, _, err := f.GetClientForContext(paths, "ctx"); err != nil {
		t.Fatal(err)
	}

	writeKubeconfig(t, second, "ctx", "https://new-server.example:6443")

	_, cfg, err := f.GetClientForContext(paths, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://new-server.example:6443" {
		t.Errorf("host = %s, want the rewritten server", cfg.Host)
	}
}

func TestGetClientForContext_BrokenRewriteErrorsThenRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, "ctx", "https://old.example:6443")

	f := NewClientFactory(false)

	if _, _, err := f.GetClientForContext([]string{path}, "ctx"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.GetClientForContext([]string{path}, "ctx"); err == nil {
		t.Fatal("expected an error, not the client for the old server, while the kubeconfig is broken")
	}

	writeKubeconfig(t, path, "ctx", "https://new.example:6443")
	_, cfg, err := f.GetClientForContext([]string{path}, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://new.example:6443" {
		t.Errorf("host = %s, want the rewritten server", cfg.Host)
	}
}
