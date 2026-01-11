```
██╗      █████╗ ███████╗██╗   ██╗███████╗██╗    ██╗██████╗  ██╗    ██╗
██║     ██╔══██╗╚══███╔╝╚██╗ ██╔╝██╔════╝██║    ██║██╔══██╗ ╚██╗   ╚██╗
██║     ███████║  ███╔╝  ╚████╔╝ █████╗  ██║ █╗ ██║██║  ██║  ╚██╗   ╚██╗
██║     ██╔══██║ ███╔╝    ╚██╔╝  ██╔══╝  ██║███╗██║██║  ██║  ██╔╝   ██╔╝
███████╗██║  ██║███████╗   ██║   ██║     ╚███╔███╔╝██████╔╝ ██╔╝   ██╔╝
╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝   ╚═╝      ╚══╝╚══╝ ╚═════╝ ╚═╝    ╚═╝
```

# lazyfwd - On-Demand Port Forwarder

A lightweight, on-demand port-forwarding proxy. Tunnels are created lazily when traffic arrives to the local port and torn down after an idle timeout. Currently supports Kubernetes services and pods.

## Features

- **Lazy port-forwarding**: Tunnels are created only when traffic arrives
- **Automatic cleanup**: Tunnels close after configurable idle timeout
- **Single binary**: No external dependencies at runtime
- **Native Kubernetes client**: Uses client-go (not kubectl subprocess)
- **Multi-context support**: Different services can use different Kubernetes contexts
- **Host-based routing**: Single port serves multiple services based on Host header or TLS SNI
- **TLS passthrough**: HTTPS services work without certificate management
- **Graceful shutdown**: Clean tunnel teardown on SIGINT/SIGTERM

## Installation

### Homebrew (macOS/Linux)

```bash
brew install atas/tap/lazyfwd
```

### Running as a Background Service

**macOS (via Homebrew):**
```bash
brew services start lazyfwd    # Start and enable auto-start on login
# Or run manually
# lazyfwd
```

**After starting it, edit the created config file `~/.lazyfwd.yaml` with an editor to add your routes.**

<details>
<summary><strong>Other useful commands</strong></summary>

```bash
brew services stop lazyfwd     # Stop the service
brew services info lazyfwd     # Show information about the service
brew services restart lazyfwd  # Restart the service
brew services list             # Check status
```

</details>

#### Logs:
Logs: `tail -f $(brew --prefix)/var/log/lazyfwd.log`

<details>
<summary><strong>Linux (systemd)</strong></summary>

```bash
# Copy the service file (included in release archives)
mkdir -p ~/.config/systemd/user
cp lazyfwd.service ~/.config/systemd/user/

# Enable and start
systemctl --user daemon-reload
systemctl --user enable --now lazyfwd

# Check status and logs
systemctl --user status lazyfwd
journalctl --user -u lazyfwd -f
```

</details>

### Go Install

```bash
go install github.com/atas/lazyfwd@latest
```

### Download Binary

Download the latest release from the [releases page](https://github.com/atas/lazyfwd/releases).

## Quick Start

1. Run lazyfwd once to generate a default config:

```bash
lazyfwd
# Creates ~/.lazyfwd.yaml with example configuration
```

2. Edit `~/.lazyfwd.yaml` with your services:

```yaml
apiVersion: lazyfwd/v1

http:
  listen: ":8989"
  idle_timeout: 60m

  k8s:
    routes:
      # https://argocd.localhost:8989
      argocd.localhost:
        context: microk8s
        namespace: argocd
        service: argocd-server
        port: 443
        scheme: https

      # http://grafana.localhost:8989
      grafana.localhost:
        context: microk8s
        namespace: observability
        service: grafana
        port: 80
```

3. Run lazyfwd:

```bash
lazyfwd
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
    Client[HTTP Request<br/>Host: app.local:8989] --> lazyfwd

    subgraph lazyfwd[lazyfwd:8989]
        direction TB
        A[1. Extract Host] --> B[2. Lookup route]
        B --> C[3. Create tunnel]
        C --> D[4. Reverse proxy]
    end

    lazyfwd -->|port-forward<br/>client-go| Pod[Kubernetes Pod]
```

1. User configures hostname → K8s service/pod mappings in YAML config
2. lazyfwd listens on a single local port (e.g., 8989)
3. When a request arrives, it inspects the `Host` header (HTTP) or SNI (TLS)
4. If no tunnel exists for that host, it creates a port-forward using client-go
5. It reverse-proxies the request through the tunnel
6. After an idle timeout (no traffic), it closes the tunnel

## Configuration

Configuration file location: `~/.lazyfwd.yaml` (or specify with `--config`)

```yaml
apiVersion: lazyfwd/v1

# verbose: true  # Enable verbose logging (or use --verbose flag)

http:
  # Listen address (handles both HTTP and HTTPS on same port)
  listen: ":8989"

  # Idle timeout before closing tunnels (Go duration format)
  idle_timeout: 60m

  k8s:
    # Path to kubeconfig (optional, defaults to ~/.kube/config)
    # kubeconfig: ~/.kube/config

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
| `service`   | Service name (lazyfwd discovers a ready pod)                |
| `pod`       | Pod name (direct targeting, no discovery)                   |
| `port`      | Service or pod port                                         |
| `scheme`    | `http` (default) or `https` - sets X-Forwarded-Proto header |

## CLI Options

```
Usage: lazyfwd [options]

Options:
  -config string
        Path to configuration file (default "~/.lazyfwd.yaml")
  -verbose
        Enable verbose logging
  -version
        Show version information
```

## Using with *.localhost

On most systems, `*.localhost` resolves to `127.0.0.1` automatically. This makes it easy to use lazyfwd without modifying `/etc/hosts`:

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
go build -o lazyfwd .
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
