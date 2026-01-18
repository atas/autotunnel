# Internal Package Architecture

This document explains how the internal packages interact with each other, starting from `main.go`.

## High-Level Architecture

### Startup Flow

```mermaid
sequenceDiagram
    participant main as main.go
    participant cfg as config
    participant mgr as tunnelmgr
    participant http as httpserver
    participant tcp as tcpserver
    participant watch as watcher

    main->>cfg: LoadConfig(path)
    cfg-->>main: *Config

    main->>mgr: NewManager(cfg)
    mgr-->>main: *Manager

    main->>http: NewServer(cfg, manager)
    http-->>main: *Server

    alt TCP routes configured
        main->>tcp: NewServer(cfg, manager)
        tcp-->>main: *Server
    end

    main->>mgr: Start()
    Note over mgr: Starts idle cleanup loop

    alt auto_reload_config enabled
        main->>watch: NewConfigWatcher(path, cfg, cliVerbose)
        watch-->>main: *ConfigWatcher
        main->>watch: Start()
    end

    loop Main run loop
        main->>http: Start() [goroutine]
        Note over http: Listens on :8989

        alt TCP server exists
            main->>tcp: Start()
            Note over tcp: Starts port listeners
        end

        main->>main: Wait for signal or reload
        alt SIGINT/SIGTERM received
            main->>main: Shutdown and exit loop
        else Config file changed
            main->>main: Shutdown all components
            main->>main: Continue loop (restart)
        end
    end
```

### Request Flow (HTTP)

```mermaid
sequenceDiagram
    participant client as Client
    participant mux as muxListener
    participant srv as Server
    participant handler as http_handler
    participant mgr as tunnelmgr
    participant tun as tunnel

    client->>mux: TCP connect to :8989
    mux->>mux: Peek first byte

    alt First byte == 0x16 (TLS)
        mux->>srv: handleTLSConnection()
        srv->>srv: extractSNI() from ClientHello
        srv->>mgr: GetOrCreateTunnel(sni, "https")
        mgr-->>srv: TunnelHandle
        srv->>tun: Start() if not running
        tun->>tun: discoverTargetPod()
        tun->>tun: createPortForwarder()
        Note over tun: SPDY port-forward to K8s
        srv->>srv: Bidirectional copy (passthrough)
    else HTTP request
        mux->>handler: Route to http.Server
        handler->>mgr: GetOrCreateTunnel(host, "http")
        mgr-->>handler: TunnelHandle
        handler->>tun: Start() if not running
        handler->>handler: ReverseProxy to 127.0.0.1:localPort
    end
```

### Request Flow (TCP)

```mermaid
sequenceDiagram
    participant client as Client
    participant tcp as tcpserver
    participant mgr as tunnelmgr
    participant tun as tunnel
    participant jump as JumpHandler
    participant k8s as Kubernetes

    client->>tcp: TCP connect to port N

    alt Route type (direct port-forward)
        tcp->>mgr: GetOrCreateTCPTunnel(port)
        mgr-->>tcp: TunnelHandle
        tcp->>tun: Start() if not running
        tun->>k8s: SPDY port-forward
        tcp->>tcp: Bidirectional copy to backend
    else Jump type (jump-host)
        tcp->>mgr: GetClientForContext()
        mgr-->>tcp: clientset, restConfig
        tcp->>jump: NewJumpHandler(route)
        jump->>jump: discoverJumpPod()
        Note over jump: Via service or direct pod
        jump->>k8s: kubectl exec socat/nc
        jump->>jump: Stream stdin/stdout
    end
```

### Config Reload Flow

The config reload mechanism uses a full-restart approach for simplicity:

```mermaid
sequenceDiagram
    participant fs as Filesystem
    participant watch as watcher
    participant cfg as config
    participant main as main.go

    fs->>watch: File change event
    Note over watch: Debounce 500ms

    watch->>cfg: LoadConfig(path)
    cfg-->>watch: *Config (validated)

    watch->>watch: Store new config
    watch->>main: Signal via ReloadChan

    main->>main: Shutdown HTTP server
    main->>main: Shutdown TCP server
    main->>main: Shutdown tunnel manager
    Note over main: Small delay for port release

    main->>main: Reinitialize with new config
    main->>main: Start all components
    Note over main: Tunnels created on-demand
```

---

## Package Details

### config

Handles YAML configuration parsing and validation.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant cfg as config.go
    participant types as types.go
    participant defaults as defaults.go
    participant validate as validate.go
    participant ops as operations.go

    caller->>cfg: LoadConfig(path)
    cfg->>defaults: DefaultConfig()
    defaults-->>cfg: *Config (with defaults)
    cfg->>cfg: yaml.Unmarshal()
    cfg->>cfg: resolveKubeconfigs()
    cfg->>validate: cfg.Validate()
    validate->>validate: Check required fields
    validate->>validate: Validate routes
    validate-->>cfg: error or nil
    cfg-->>caller: *Config, error

    Note over ops: PrintRoutes(), PrintTCPRoutes()
    Note over ops: PrintJumpRoutes()
```

| File | Purpose |
|------|---------|
| `config.go` | `LoadConfig()`, kubeconfig resolution |
| `types.go` | All struct definitions (`Config`, `HTTPConfig`, `K8sRouteConfig`, etc.) |
| `defaults.go` | `DefaultConfig()` with sensible defaults |
| `validate.go` | `Validate()` method, route validation |
| `operations.go` | `PrintRoutes()`, `ShouldAutoReload()`, helper methods |
| `execpath.go` | `ExpandExecPath()` for systemd/launchd PATH issues |

---

### httpserver

Handles HTTP reverse proxy and TLS passthrough on a single port.

```mermaid
sequenceDiagram
    participant conn as net.Conn
    participant srv as server.go
    participant mux as mux_listener.go
    participant http as http_handler.go
    participant tls as tls_passthrough.go
    participant err as tls_error_*.go

    conn->>srv: Accept()
    srv->>mux: newPeekConn(conn)
    mux->>mux: isTLS() - peek first byte

    alt TLS (0x16)
        srv->>tls: handleTLSConnection()
        tls->>tls: extractSNI()
        alt Error occurs
            tls->>err: sendTLSErrorPage()
            err->>err: Generate self-signed cert
            err->>err: TLS handshake + HTML error
        else Success
            tls->>tls: Bidirectional TCP copy
        end
    else HTTP
        srv->>mux: Route to httpConns channel
        mux->>http: ServeHTTP(w, r)
        http->>http: Create ReverseProxy
        http->>http: Set X-Forwarded-* headers
    end
```

| File | Purpose |
|------|---------|
| `server.go` | `Server` struct, `Start()`, `Shutdown()`, connection routing |
| `mux_listener.go` | `peekConn` (peek without consuming), `muxListener` (routes HTTP vs TLS) |
| `http_handler.go` | `ServeHTTP()` - reverse proxy with header injection |
| `tls_passthrough.go` | `handleTLSConnection()`, `extractSNI()` - raw TCP forwarding |
| `tls_error_handler.go` | `sendTLSErrorPage()` - user-friendly TLS error pages |
| `tls_error_cert.go` | Dynamic self-signed certificate generation for error pages |
| `tls_error_page.go` | HTML template for TLS error pages |

---

### tcpserver

Handles raw TCP port forwarding and socat/jump-host connections.

```mermaid
sequenceDiagram
    participant port as Port Listener
    participant srv as server.go
    participant jump as jump_handler.go
    participant k8s as Kubernetes

    srv->>srv: Start()
    loop For each configured port
        srv->>srv: startListener(port, type)
        srv->>port: net.Listen("tcp", addr)
    end

    port->>srv: Accept()

    alt listenerTypeRoute
        srv->>srv: handleConnection()
        srv->>srv: manager.GetOrCreateTCPTunnel()
        srv->>srv: io.Copy bidirectional
    else listenerTypeJump
        srv->>srv: handleJumpConnection()
        srv->>jump: NewJumpHandler(route)
        jump->>jump: discoverJumpPod()
        alt Via Service
            jump->>k8s: Get Service
            jump->>k8s: List Pods by selector
            jump->>jump: Select ready pod
        else Via Pod
            jump->>jump: Use pod name directly
        end
        jump->>k8s: POST /exec (socat/nc command)
        jump->>jump: Stream via SPDY
    end
```

| File | Purpose |
|------|---------|
| `server.go` | `Server` struct, listener management, connection handling |
| `jump_handler.go` | `JumpHandler` - kubectl exec + socat/nc for jump-host routing |
| `types.go` | `Manager` interface for dependency injection |

---

### tunnel

Individual port-forward tunnel using client-go SPDY. Uses `k8sutil` for pod/service resolution.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant tun as tunnel.go
    participant pf as port_forward.go
    participant k8sutil as k8sutil
    participant ops as operations.go
    participant k8s as Kubernetes API

    caller->>tun: NewTunnel(hostname, cfg, ...)
    tun-->>caller: *Tunnel (StateIdle)

    caller->>tun: Start(ctx)
    alt StateRunning
        tun-->>caller: nil (already running)
    else StateStarting
        tun->>tun: awaitReady(ctx)
        Note over tun: Wait for concurrent Start()
    else StateIdle
        tun->>tun: state = StateStarting
        tun->>pf: startPortForward()
    end

    pf->>pf: discoverTargetPod()
    alt Service configured
        pf->>k8sutil: GetService()
        k8sutil->>k8s: Get Service
        pf->>k8sutil: ResolveServicePort()
        pf->>k8sutil: FindReadyPod()
        k8sutil->>k8s: List Pods (selector)
        alt Named port
            pf->>k8sutil: ResolveNamedPort()
        end
    else Pod configured
        pf->>pf: Use pod name directly
    end

    pf->>pf: createPortForwarder()
    pf->>k8s: SPDY upgrade for port-forward
    pf->>pf: waitForReady()

    alt Ready
        pf->>tun: state = StateRunning
        pf->>pf: monitorErrors() [goroutine]
    else Error/Timeout
        pf->>tun: state = StateFailed
        pf->>tun: lastError = err
    end

    caller->>ops: Touch()
    Note over ops: Update lastAccess time

    caller->>ops: IdleDuration()
    ops-->>caller: time since lastAccess

    caller->>tun: Stop()
    tun->>tun: close(stopChan)
    tun->>tun: state = StateIdle
```

| File | Purpose |
|------|---------|
| `tunnel.go` | `Tunnel` struct, state machine, `Start()`/`Stop()`/`awaitReady()` |
| `port_forward.go` | `discoverTargetPod()`, `createPortForwarder()`, `waitForReady()` - uses `k8sutil` for resolution |
| `operations.go` | `Touch()`, `IdleDuration()`, `LocalPort()`, `Scheme()`, accessor methods |

---

### tunnelmgr

Manages tunnel lifecycle and idle cleanup. Uses `k8sutil.ClientFactory` for K8s client caching.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant mgr as manager.go
    participant ops as operations.go
    participant tcp_ops as tcp_operations.go
    participant cf as k8sutil.ClientFactory
    participant dyn as dynamic_route.go
    participant tun as tunnel

    caller->>mgr: NewManager(cfg)
    Note over mgr: Creates k8sutil.ClientFactory
    mgr-->>caller: *Manager

    caller->>mgr: Start()
    mgr->>ops: idleCleanupLoop() [goroutine]

    caller->>ops: GetOrCreateTunnel(hostname, scheme)
    ops->>ops: Check tunnels map
    alt Not found in static routes
        ops->>dyn: ParseDynamicHostname()
        dyn-->>ops: Resolved route or nil
    end
    ops->>cf: GetClientForContext(paths, ctx)
    cf-->>ops: clientset, restConfig
    ops->>tun: tunnelFactory(...)
    ops-->>caller: TunnelHandle

    caller->>tcp_ops: GetOrCreateTCPTunnel(port)
    tcp_ops->>tcp_ops: Check tcpTunnels map
    tcp_ops->>cf: GetClientForContext(paths, ctx)
    tcp_ops->>tun: tunnelFactory(...)
    tcp_ops-->>caller: TunnelHandle

    loop Every 30 seconds
        ops->>ops: cleanupIdleTunnels()
        ops->>ops: Check HTTP tunnels
        ops->>ops: Check TCP tunnels
        Note over ops: Stop tunnels exceeding idle_timeout
    end

    caller->>mgr: Shutdown()
    mgr->>mgr: Stop all tunnels
    mgr->>cf: Clear()
```

| File | Purpose |
|------|---------|
| `manager.go` | `Manager` struct, `NewManager()`, `Start()`, `Shutdown()`, owns `k8sutil.ClientFactory` |
| `operations.go` | `GetOrCreateTunnel()`, `idleCleanupLoop()`, HTTP tunnel management |
| `tcp_operations.go` | `GetOrCreateTCPTunnel()`, TCP tunnel management |
| `dynamic_route.go` | `ParseDynamicHostname()` - pattern-based route resolution |
| `types.go` | `TunnelHandle` interface, `TunnelFactory` type |

---

### watcher

Watches config file for changes and signals main.go to restart.

```mermaid
sequenceDiagram
    participant fs as Filesystem
    participant w as watcher.go
    participant cfg as config
    participant main as main.go

    Note over w: NewConfigWatcher()
    w->>fs: fsnotify.Add(configPath)

    Note over w: Start()
    w->>w: watchLoop() [goroutine]

    loop Watch events
        fs->>w: Write/Create/Rename event

        alt Rename (vim/nano atomic save)
            w->>fs: Remove + Add watch
            Note over w: Re-watch new inode
        end

        w->>w: Debounce timer (500ms)
        w->>w: reloadConfig()
        w->>cfg: LoadConfig(path)
        cfg-->>w: *Config (validated)

        w->>w: Store validated config
        w->>main: Signal ReloadChan
        Note over main: Triggers restart loop
    end

    Note over w: Stop()
    w->>w: close(stopChan)
    w->>fs: watcher.Close()
```

| File | Purpose |
|------|---------|
| `watcher.go` | `ConfigWatcher` struct, fsnotify integration, debouncing, `ReloadChan` signaling |

---

### k8sutil

Shared Kubernetes client utilities extracted from tunnelmgr for reuse.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant cf as ClientFactory
    participant client as client.go
    participant pod as pod.go
    participant svc as service.go
    participant k8s as Kubernetes API

    caller->>cf: GetClientForContext(paths, ctx)
    cf->>cf: Check cache
    alt Cached
        cf-->>caller: clientset, restConfig
    else Not cached
        cf->>k8s: Build REST config
        cf->>k8s: Create clientset
        cf->>cf: Cache client
        cf-->>caller: clientset, restConfig
    end

    caller->>svc: GetService(ctx, clientset, ns, name)
    svc->>k8s: Get Service
    svc-->>caller: *Service

    caller->>svc: ResolveServicePort(svc, port)
    svc-->>caller: targetPort, portName

    caller->>pod: FindReadyPod(ctx, clientset, ns, labels, svc)
    pod->>k8s: List Pods (selector, Running)
    pod->>pod: Find first ready pod
    pod-->>caller: *Pod

    caller->>pod: WaitForPodReady(ctx, clientset, ns, name, timeout)
    loop Poll until ready
        pod->>k8s: Get Pod
        pod->>pod: Check Ready condition
    end
    pod-->>caller: nil or error
```

| File | Purpose |
|------|---------|
| `client.go` | `ClientFactory` - caches Kubernetes clients per context |
| `pod.go` | `FindReadyPod()`, `WaitForPodReady()` - pod discovery utilities |
| `service.go` | `GetService()`, `ResolveServicePort()`, `ResolveNamedPort()` - service utilities |

---

### netutil

Shared network utilities for connection handling.

| File | Purpose |
|------|---------|
| `copy.go` | `BidirectionalCopy()` - copies data between two connections with proper TCP half-close handling |

---

## Dependency Graph

```
main.go
├── config          (loaded first, no internal deps)
├── k8sutil         (depends on: k8s client-go)
├── netutil         (no dependencies)
├── tunnelmgr       (depends on: config, tunnel, k8sutil)
│   └── tunnel      (depends on: config, k8sutil)
├── httpserver      (depends on: config, tunnelmgr, netutil)
├── tcpserver       (depends on: config, tunnelmgr, k8sutil, netutil)
└── watcher         (depends on: config only, signals main.go via ReloadChan)
```

## State Machines

### Tunnel States

```
    ┌─────────┐
    │  Idle   │◄─────────────────┐
    └────┬────┘                  │
         │ Start()               │ Stop() or error
         ▼                       │
    ┌─────────┐                  │
    │Starting │──────────────────┤
    └────┬────┘   timeout/fail   │
         │                       │
    ┌────┴────┐                  │
    │         │                  │
    ▼         ▼                  │
 ready     error                 │
    │         │                  │
    ▼         ▼                  │
┌───────┐ ┌────────┐             │
│Running│ │ Failed │─────────────┤
└───┬───┘ └────────┘             │
    │         ▲                  │
    │ Stop()  │ error            │
    ▼         │                  │
┌─────────┐   │                  │
│Stopping │───┴──────────────────┘
└─────────┘
```

**Concurrent Start() Handling:**

When multiple goroutines call `Start()` concurrently:
1. First caller transitions from `Idle` → `Starting` and performs startup
2. Subsequent callers see `StateStarting` and call `awaitReady()`
3. `awaitReady()` polls state every 100ms until:
   - `StateRunning`: returns success
   - `StateFailed`: returns the stored error
   - `StateIdle`: retries `Start()` (first caller failed, cleaned up)

**State Preservation:**

The manager only removes tunnels from its map when they are in `StateFailed` or `StateStopping`.
Tunnels in `StateStarting`, `StateIdle`, or `StateRunning` are preserved to handle concurrent access.

### Connection Routing

```
Incoming Connection (:8989)
         │
         ▼
    ┌─────────┐
    │Peek byte│
    └────┬────┘
         │
    ┌────┴────┐
    │         │
    ▼         ▼
  0x16      other
  (TLS)     (HTTP)
    │         │
    ▼         ▼
┌───────┐ ┌───────┐
│Extract│ │Reverse│
│  SNI  │ │ Proxy │
└───┬───┘ └───┬───┘
    │         │
    ▼         ▼
┌───────────────────┐
│GetOrCreateTunnel()│
└─────────┬─────────┘
          │
          ▼
    ┌───────────┐
    │Forward to │
    │ localhost │
    │   :port   │
    └───────────┘
```
