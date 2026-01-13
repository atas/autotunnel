```
 █████╗ ██╗   ██╗████████╗ ██████╗ ████████╗██╗   ██╗███╗   ██╗███╗   ██╗███████╗██╗
██╔══██╗██║   ██║╚══██╔══╝██╔═══██╗╚══██╔══╝██║   ██║████╗  ██║████╗  ██║██╔════╝██║
███████║██║   ██║   ██║   ██║   ██║   ██║   ██║   ██║██╔██╗ ██║██╔██╗ ██║█████╗  ██║
██╔══██║██║   ██║   ██║   ██║   ██║   ██║   ██║   ██║██║╚██╗██║██║╚██╗██║██╔══╝  ██║
██║  ██║╚██████╔╝   ██║   ╚██████╔╝   ██║   ╚██████╔╝██║ ╚████║██║ ╚████║███████╗███████╗
╚═╝  ╚═╝ ╚═════╝    ╚═╝    ╚═════╝    ╚═╝    ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═══╝╚══════╝╚══════╝
```

# autotunnel - On-Demand Port Forwarder & Tunnel

A lightweight, auto and on-demand (lazy) port-forwarding proxy to Kubernetes. Tunnels are created lazily when traffic arrives to the local port and torn down after an idle timeout. Currently supports Kubernetes services and pods.

## Summary

Connect to Kubernetes services and pods with friendly URLs automatically:

* `https://argocd.localhost:8989` — Pre-configured route
* `http://nginx-80.svc.default.ns.microk8s.cx.k8s.localhost:8989` — Dynamic routing (no config needed)

### Features

* **On-demand tunneling** - port-forwards are created automatically when traffic arrives
* **HTTP and HTTPS support** - HTTP reverse proxy with `X-Forwarded-*` headers, TLS passthrough for HTTPS
* **K8s Services and Pods** - target either directly or let it discover pods via service selectors
* **Protocol multiplexing** - serve both HTTP and HTTPS on a single port
* **Idle cleanup** - tunnels automatically close after configurable idle timeout
* **Native client-go** - uses Kubernetes client library directly (no kubectl subprocess)


## Installation

### Homebrew (macOS/Linux)

```bash
brew install atas/tap/autotunnel
```

### Running as a Background Service

**macOS (via Homebrew):**
```bash
brew services start autotunnel    # Start and enable auto-start on login
# Or run manually
# autotunnel
```

**After starting it, edit the created config file `~/.autotunnel.yaml` with an editor to add your routes.**

<details>
<summary><strong>Other useful commands</strong></summary>

```bash
brew services stop autotunnel     # Stop the service
brew services info autotunnel     # Show information about the service
brew services restart autotunnel  # Restart the service
brew services list             # Check status
```

</details>

#### Logs
```bash
tail -f $(brew --prefix)/var/log/autotunnel.log
```

<details>
<summary><strong>Linux (systemd)</strong></summary>

```bash
# Copy the service file (included in release archives)
mkdir -p ~/.config/systemd/user
cp autotunnel.service ~/.config/systemd/user/

# Enable and start
systemctl --user daemon-reload
systemctl --user enable --now autotunnel

# Check status and logs
systemctl --user status autotunnel
journalctl --user -u autotunnel -f
```

</details>

### Go Install

```bash
go install github.com/atas/autotunnel@latest
```

### Download Binary

Download the latest release from the [releases page](https://github.com/atas/autotunnel/releases).

## Quick Start

1. Run autotunnel, either manually or as a service. It will generate a default config file:

```bash
brew services start autotunnel #or just run `autotunnel` manually
# Creates ~/.autotunnel.yaml with example configuration
```

2. Edit `~/.autotunnel.yaml` with your services:

3. It will auto-reload unless port changes.

```yaml
apiVersion: autotunnel/v1

# Verbose logging (can also use --verbose flag with higher priority)
# verbose: false

# Auto-reload on file changes. Any changes need `brew services restart autotunnel` while it is false.
auto_reload_config: true

# Common paths (/usr/local/bin, /opt/homebrew/bin, etc.) are added automatically.
# Add custom paths here if your credential plugin is in a non-standard location.
# exec_path:
#   - /custom/path/to/binaries

http:
   # Listen address - handles both HTTP and HTTPS (TLS passthrough) on same port
   listen: "127.0.0.1:8989" # Port changes require: brew services restart autotunnel

   # Idle timeout before closing tunnels (Go duration format)
   # After this duration of no traffic, the tunnel will be closed
   idle_timeout: 60m

   k8s:
      # Path(s) to kubeconfig. Supports colon-separated paths like $KUBECONFIG.
      # Tries to use $KUBECONFIG env var as well but that's not available in the service
      # then defaults to ~/.kube/config
      # kubeconfig: ~/.kube/config:~/.kube/prod-config

      # Dynamic routing: access any K8s service or pod without pre-configuring routes
      # Formats:
      #   Service: {service}-{port}.svc.{namespace}.ns.{context}.cx.{dynamic_host}
      #   Pod:     {pod}-{port}.pod.{namespace}.ns.{context}.cx.{dynamic_host}
      # Examples:
      #   http://nginx-80.svc.default.ns.my-cluster-context.cx.k8s.localhost:8989
      #   https://argocd-server-443.svc.argocd.ns.my-cluster-context.cx.k8s.localhost:8989
      #   http://nginx-2fxac-80.pod.default.ns.my-cluster-context.cx.k8s.localhost:8989
      dynamic_host: k8s.localhost

      routes:
         # Static routes (take priority over dynamic routing)

         # https://argocd.localhost:8989 (also supports http http://argocd.localhost:8989)
         argocd.localhost:
           context: my-cluster-context # Kubernetes context name from kubeconfig
           namespace: argocd           # Kubernetes namespace
           service: argocd-server      # Kubernetes service name
           port: 443                   # Service port (automatically resolves to container targetPort)
           scheme: https               # Default is http.
```

3. Run autotunnel:

```bash
autotunnel
```

4. Access your services:

```bash
# ArgoCD (TLS passthrough)
curl -k https://argocd.localhost:8989/

# Grafana
curl http://grafana.localhost:8989/
```

## How It Works (k8s example)

```mermaid
flowchart LR
    Client[HTTP Request<br/>Host: app.local:8989] --> autotunnel

    subgraph autotunnel[autotunnel:8989]
        direction TB
        A[1. Extract Host] --> B[2. Lookup route]
        B --> C[3. Create tunnel]
        C --> D[4. Reverse proxy]
    end

    autotunnel -->|port-forward<br/>client-go| Pod[Kubernetes Pod]
```

1. User configures hostname → K8s service/pod mappings in YAML config
2. autotunnel listens on a single local port (e.g., 8989)
3. When a request arrives, it inspects the `Host` header (HTTP) or SNI (TLS)
4. If no tunnel exists for that host, it creates a port-forward using client-go
5. It reverse-proxies the request through the tunnel
6. After an idle timeout (no traffic), it closes the tunnel

## Configuration

Configuration file location: `~/.autotunnel.yaml` (or specify with `--config`)

```yaml
apiVersion: autotunnel/v1

# verbose: true  # Enable verbose logging (or use --verbose flag)

# Auto-reload on file changes (disable requires: brew services restart autotunnel)
auto_reload_config: true

# Additional paths for exec credential plugins (e.g., aws-iam-authenticator, gcloud)
# Common paths (/usr/local/bin, /opt/homebrew/bin, etc.) are added automatically.
# exec_path:
#   - /custom/path/to/binaries

http:
  # Listen address (handles both HTTP and HTTPS on same port)
  listen: "127.0.0.1:8989"  # Port changes require: brew services restart autotunnel

  # Idle timeout before closing tunnels (Go duration format)
  idle_timeout: 60m

  k8s:
    # Path(s) to kubeconfig. Supports colon-separated paths like $KUBECONFIG.
    # Defaults to ~/.kube/config
    # kubeconfig: ~/.kube/config:~/.kube/prod-config

    routes:
      # Route via service (discovers pod automatically)
      argocd.localhost:
        context: microk8s           # Kubernetes context from kubeconfig
        namespace: argocd           # Kubernetes namespace
        service: argocd-server      # Kubernetes service name
        port: 443                   # Service port
        scheme: https               # Sets X-Forwarded-Proto header

      grafana.localhost:
        context: microk8s
        namespace: observability
        service: grafana
        port: 3000

      # Route directly to a pod (no pod discovery)
      debug.localhost:
        context: microk8s
        namespace: default
        pod: my-debug-pod           # Pod name (use instead of service)
        port: 8080
```

### Route Options

Each route requires either `service` or `pod` (mutually exclusive):

| Field       | Description                                                 |
| ----------- | ----------------------------------------------------------- |
| `context`   | Kubernetes context name from kubeconfig                     |
| `namespace` | Kubernetes namespace                                        |
| `service`   | Service name (autotunnel discovers a ready pod)             |
| `pod`       | Pod name (direct targeting, no discovery)                   |
| `port`      | Service or pod port                                         |
| `scheme`    | `http` (default) or `https` - sets X-Forwarded-Proto header |

## CLI Options

```
Usage: autotunnel [options]

Options:
  -config string
        Path to configuration file (default "~/.autotunnel.yaml")
  -verbose
        Enable verbose logging
  -version
        Show version information
```

## Using with *.localhost

On most systems, `*.localhost` resolves to `127.0.0.1` automatically. This makes it easy to use autotunnel without modifying `/etc/hosts`:

```yaml
http:
  k8s:
    routes:
      myapp.localhost:
        # ...
```

Then access via: `http://myapp.localhost:8989/`

## Using with /etc/hosts

For custom hostnames, add entries to `/etc/hosts`:

```
127.0.0.1  myapp.local
127.0.0.1  api.local
```

## Development

### Building

```bash
go build -o autotunnel .
```

### Running Tests

```bash
go test -v ./...
```

### Creating a Release

Releases are automated via GoReleaser. Push a tag to trigger:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## License

MIT License - see [LICENSE](LICENSE) for details.
