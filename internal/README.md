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
        main->>watch: NewConfigWatcher(path, cfg, manager)
        watch-->>main: *ConfigWatcher
        main->>watch: Start()
        main->>watch: SetTCPServer(tcpServer)
    end

    main->>http: Start() [goroutine]
    Note over http: Listens on :8989

    alt TCP server exists
        main->>tcp: Start()
        Note over tcp: Starts port listeners
    end

    main->>main: Wait for SIGINT/SIGTERM
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
    else Socat type (jump-host)
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

```mermaid
sequenceDiagram
    participant fs as Filesystem
    participant watch as watcher
    participant cfg as config
    participant mgr as tunnelmgr
    participant tcp as tcpserver

    fs->>watch: File change event
    Note over watch: Debounce 500ms

    watch->>cfg: LoadConfig(path)
    cfg-->>watch: *Config (new)

    watch->>mgr: UpdateConfig(newConfig)
    Note over mgr: Stop tunnels for removed routes

    alt TCP server registered
        watch->>tcp: UpdateConfig(newConfig)
        Note over tcp: Stop/start listeners as needed
    end
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
    Note over ops: PrintSocatRoutes()
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
    else listenerTypeSocat
        srv->>srv: handleSocatConnection()
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
| `server.go` | `Server` struct, listener management, connection handling, `UpdateConfig()` |
| `jump_handler.go` | `JumpHandler` - kubectl exec + socat/nc for jump-host routing |
| `types.go` | `Manager` interface for dependency injection |

---

### tunnel

Individual port-forward tunnel using client-go SPDY.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant tun as tunnel.go
    participant pf as port_forward.go
    participant ops as operations.go
    participant k8s as Kubernetes API

    caller->>tun: NewTunnel(hostname, cfg, ...)
    tun-->>caller: *Tunnel (StateIdle)

    caller->>tun: Start(ctx)
    tun->>tun: state = StateStarting
    tun->>pf: startPortForward()

    pf->>pf: discoverTargetPod()
    alt Service configured
        pf->>k8s: Get Service
        pf->>pf: Resolve port mapping
        pf->>k8s: List Pods (label selector)
        pf->>pf: findReadyPod()
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
| `tunnel.go` | `Tunnel` struct, state machine (`Idle`→`Starting`→`Running`→`Stopping`), `Start()`/`Stop()` |
| `port_forward.go` | `discoverTargetPod()`, `createPortForwarder()`, `waitForReady()`, pod/service resolution |
| `operations.go` | `Touch()`, `IdleDuration()`, `LocalPort()`, `Scheme()`, accessor methods |

---

### tunnelmgr

Manages tunnel lifecycle, K8s client caching, and idle cleanup.

```mermaid
sequenceDiagram
    participant caller as Caller
    participant mgr as manager.go
    participant ops as operations.go
    participant tcp_ops as tcp_operations.go
    participant k8s as k8s_client.go
    participant dyn as dynamic_route.go
    participant tun as tunnel

    caller->>mgr: NewManager(cfg)
    mgr-->>caller: *Manager

    caller->>mgr: Start()
    mgr->>ops: idleCleanupLoop() [goroutine]

    caller->>ops: GetOrCreateTunnel(hostname, scheme)
    ops->>ops: Check tunnels map
    alt Not found in static routes
        ops->>dyn: ParseDynamicHostname()
        dyn-->>ops: Resolved route or nil
    end
    ops->>k8s: getClientsetAndConfig()
    k8s-->>ops: clientset, restConfig
    ops->>tun: tunnelFactory(...)
    ops-->>caller: TunnelHandle

    caller->>tcp_ops: GetOrCreateTCPTunnel(port)
    tcp_ops->>tcp_ops: Check tcpTunnels map
    tcp_ops->>k8s: getClientsetAndConfig()
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
    mgr->>mgr: Clear k8s clients
```

| File | Purpose |
|------|---------|
| `manager.go` | `Manager` struct, `NewManager()`, `Start()`, `Shutdown()`, `UpdateConfig()` |
| `operations.go` | `GetOrCreateTunnel()`, `idleCleanupLoop()`, HTTP tunnel management |
| `tcp_operations.go` | `GetOrCreateTCPTunnel()`, TCP tunnel management |
| `k8s_client.go` | `getClientsetAndConfig()` - cached K8s client per context |
| `dynamic_route.go` | `ParseDynamicHostname()` - pattern-based route resolution |
| `types.go` | `TunnelHandle` interface, `TunnelFactory` type |

---

### watcher

Watches config file for changes and triggers hot reload.

```mermaid
sequenceDiagram
    participant fs as Filesystem
    participant w as watcher.go
    participant cfg as config
    participant mgr as Manager
    participant tcp as TCPServer

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
        cfg-->>w: *Config (new)

        w->>mgr: UpdateConfig(newConfig)

        alt tcpServer registered
            w->>tcp: UpdateConfig(newConfig)
        end
    end

    Note over w: Stop()
    w->>w: close(stopChan)
    w->>fs: watcher.Close()
```

| File | Purpose |
|------|---------|
| `watcher.go` | `ConfigWatcher` struct, fsnotify integration, debouncing, `reloadConfig()` |

---

## Dependency Graph

```
main.go
├── config          (loaded first, no internal deps)
├── tunnelmgr       (depends on: config, tunnel)
│   └── tunnel      (depends on: config, k8s client-go)
├── httpserver      (depends on: config, tunnelmgr)
├── tcpserver       (depends on: config, tunnelmgr)
└── watcher         (depends on: config, tunnelmgr, tcpserver)
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
         │ ready                 │
         ▼                       │
    ┌─────────┐                  │
    │ Running │──────────────────┤
    └────┬────┘   Stop()         │
         │                       │
         ▼                       │
    ┌─────────┐                  │
    │Stopping │──────────────────┘
    └─────────┘
```

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
