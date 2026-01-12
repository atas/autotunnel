package tunnelmgr

import (
	"testing"
)

func TestParseDynamicHostname(t *testing.T) {
	tests := []struct {
		name        string
		hostname    string
		dynamicHost string
		scheme      string
		wantService string
		wantPod     string
		wantPort    int
		wantNS      string
		wantCtx     string
		wantScheme  string
		wantOK      bool
	}{
		// Service tests
		{
			name:        "valid simple service",
			hostname:    "nginx-80.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantService: "nginx",
			wantPort:    80,
			wantNS:      "default",
			wantCtx:     "microk8s",
			wantScheme:  "http",
			wantOK:      true,
		},
		{
			name:        "valid service with hyphens",
			hostname:    "argocd-server-443.svc.argocd.ns.my-cluster.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "https",
			wantService: "argocd-server",
			wantPort:    443,
			wantNS:      "argocd",
			wantCtx:     "my-cluster",
			wantScheme:  "https",
			wantOK:      true,
		},
		{
			name:        "valid complex service name",
			hostname:    "cert-manager-webhook-9402.svc.cert-manager.ns.prod-cluster.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantService: "cert-manager-webhook",
			wantPort:    9402,
			wantNS:      "cert-manager",
			wantCtx:     "prod-cluster",
			wantScheme:  "http",
			wantOK:      true,
		},
		{
			name:        "valid service with port 3000",
			hostname:    "grafana-3000.svc.monitoring.ns.docker-desktop.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantService: "grafana",
			wantPort:    3000,
			wantNS:      "monitoring",
			wantCtx:     "docker-desktop",
			wantScheme:  "http",
			wantOK:      true,
		},
		// Pod tests
		{
			name:        "valid simple pod",
			hostname:    "nginx-2fxac-80.pod.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantPod:     "nginx-2fxac",
			wantPort:    80,
			wantNS:      "default",
			wantCtx:     "microk8s",
			wantScheme:  "http",
			wantOK:      true,
		},
		{
			name:        "valid pod with complex name",
			hostname:    "argocd-server-dc21sa-443.pod.argocd.ns.my-cluster.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "https",
			wantPod:     "argocd-server-dc21sa",
			wantPort:    443,
			wantNS:      "argocd",
			wantCtx:     "my-cluster",
			wantScheme:  "https",
			wantOK:      true,
		},
		{
			name:        "valid pod in kube-system",
			hostname:    "coredns-abc123-8080.pod.kube-system.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantPod:     "coredns-abc123",
			wantPort:    8080,
			wantNS:      "kube-system",
			wantCtx:     "microk8s",
			wantScheme:  "http",
			wantOK:      true,
		},
		// Invalid tests
		{
			name:        "invalid - missing port",
			hostname:    "nginx.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - wrong suffix",
			hostname:    "nginx-80.svc.default.ns.microk8s.cx.other.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - missing .ns. segment",
			hostname:    "nginx-80.svc.default.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - missing .svc. or .pod. segment",
			hostname:    "nginx-80.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - missing .cx. segment",
			hostname:    "nginx-80.svc.default.ns.microk8s.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - non-numeric port",
			hostname:    "nginx-abc.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - port out of range",
			hostname:    "nginx-99999.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - empty dynamicHost",
			hostname:    "nginx-80.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - empty hostname",
			hostname:    "",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "invalid - port zero",
			hostname:    "nginx-0.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
		{
			name:        "valid service - high port number",
			hostname:    "nginx-65535.svc.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantService: "nginx",
			wantPort:    65535,
			wantNS:      "default",
			wantCtx:     "microk8s",
			wantScheme:  "http",
			wantOK:      true,
		},
		{
			name:        "valid service - namespace with hyphens",
			hostname:    "svc-8080.svc.kube-system.ns.ctx.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantService: "svc",
			wantPort:    8080,
			wantNS:      "kube-system",
			wantCtx:     "ctx",
			wantScheme:  "http",
			wantOK:      true,
		},
		{
			name:        "invalid pod - missing port",
			hostname:    "nginx-2fxac.pod.default.ns.microk8s.cx.k8s.localhost",
			dynamicHost: "k8s.localhost",
			scheme:      "http",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, ok := ParseDynamicHostname(tt.hostname, tt.dynamicHost, tt.scheme)

			if ok != tt.wantOK {
				t.Errorf("ParseDynamicHostname() ok = %v, want %v", ok, tt.wantOK)
				return
			}

			if !tt.wantOK {
				if cfg != nil {
					t.Errorf("ParseDynamicHostname() returned non-nil config for invalid input")
				}
				return
			}

			if cfg.Service != tt.wantService {
			t.Errorf("Service = %q, want %q", cfg.Service, tt.wantService)
		}
		if cfg.Pod != tt.wantPod {
			t.Errorf("Pod = %q, want %q", cfg.Pod, tt.wantPod)
		}
		if cfg.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", cfg.Port, tt.wantPort)
			}
			if cfg.Namespace != tt.wantNS {
				t.Errorf("Namespace = %q, want %q", cfg.Namespace, tt.wantNS)
			}
			if cfg.Context != tt.wantCtx {
				t.Errorf("Context = %q, want %q", cfg.Context, tt.wantCtx)
			}
			if cfg.Scheme != tt.wantScheme {
				t.Errorf("Scheme = %q, want %q", cfg.Scheme, tt.wantScheme)
			}
		})
	}
}
