package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/atas/autotunnel/internal/httpserver"
	"github.com/atas/autotunnel/internal/tcpserver"
	"github.com/atas/autotunnel/internal/tunnelmgr"
	"github.com/atas/autotunnel/internal/watcher"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type appComponents struct {
	cfg        *config.Config
	manager    *tunnelmgr.Manager
	httpServer *httpserver.Server
	tcpServer  *tcpserver.Server
}

func main() {
	var configPath string
	var verbose bool
	var showVersion bool

	home, _ := os.UserHomeDir()
	defaultConfig := filepath.Join(home, ".autotunnel.yaml")

	flag.StringVar(&configPath, "config", defaultConfig, "Path to configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.Parse()

	if showVersion {
		fmt.Printf("autotunnel version %s (commit: %s, built: %s)\n", version, commit, date)
		return
	}

	printBanner()

	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("[autotunnel] ")

	if !config.FileExists(configPath) {
		fmt.Println("-----------------------------------------------------------------------------")
		fmt.Printf("Config file not found, creating: %s\n", configPath)

		if err := config.CreateDefaultConfig(configPath); err != nil {
			log.Fatalf("Failed to create config file: %v", err)
		}

		fmt.Printf("Created: %s\n", configPath)
	}

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Set up config watcher (persists across restarts)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configPath, err)
	}

	var configWatcher *watcher.ConfigWatcher
	if cfg.ShouldAutoReload() {
		configWatcher, err = watcher.NewConfigWatcher(configPath, cfg, verbose)
		if err != nil {
			log.Printf("Warning: Failed to start config watcher: %v", err)
		} else {
			configWatcher.Start()
			defer configWatcher.Stop()
		}
	}

	// Main run loop - restart on config changes
	for {
		app, err := initializeApp(configPath, verbose, configWatcher)
		if err != nil {
			log.Fatalf("Failed to initialize: %v", err)
		}

		printConfigInfo(configPath, app.cfg)

		// Start servers
		app.manager.Start()

		serverErrChan := make(chan error, 1)
		go func() {
			if err := app.httpServer.Start(); err != nil {
				serverErrChan <- err
			}
		}()

		if app.tcpServer != nil {
			if err := app.tcpServer.Start(); err != nil {
				shutdownApp(app, context.Background())
				log.Fatalf("Failed to start TCP server: %v", err)
			}
		}

		fmt.Printf("Listening on %s\n", app.cfg.HTTP.ListenAddr)

		// Wait for signal or config reload
		shouldExit := false
		select {
		case sig := <-sigChan:
			log.Printf("Received signal %v, shutting down...", sig)
			shouldExit = true

		case err := <-serverErrChan:
			log.Fatalf("Server error: %v", err)

		case <-getReloadChan(configWatcher):
			log.Println("Config changed, restarting...")
		}

		// Shutdown current instance
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		shutdownApp(app, ctx)
		cancel()

		if shouldExit {
			break
		}

		// Small delay before restart to let ports release
		time.Sleep(100 * time.Millisecond)
	}

	log.Println("Shutdown complete")
}

func printBanner() {
	fmt.Println(`
 █████╗ ██╗   ██╗████████╗ ██████╗ ████████╗██╗   ██╗███╗   ██╗███╗   ██╗███████╗██╗
██╔══██╗██║   ██║╚══██╔══╝██╔═══██╗╚══██╔══╝██║   ██║████╗  ██║████╗  ██║██╔════╝██║
███████║██║   ██║   ██║   ██║   ██║   ██║   ██║   ██║██╔██╗ ██║██╔██╗ ██║█████╗  ██║
██╔══██║██║   ██║   ██║   ██║   ██║   ██║   ██║   ██║██║╚██╗██║██║╚██╗██║██╔══╝  ██║
██║  ██║╚██████╔╝   ██║   ╚██████╔╝   ██║   ╚██████╔╝██║ ╚████║██║ ╚████║███████╗███████╗
╚═╝  ╚═╝ ╚═════╝    ╚═╝    ╚═════╝    ╚═╝    ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═══╝╚══════╝╚══════╝

On-demand Port Forwarding
⭐🌟⭐ Please give the repo a star if useful ⭐🌟⭐
https://github.com/atas/autotunnel`)
}

func initializeApp(configPath string, cliVerbose bool, configWatcher *watcher.ConfigWatcher) (*appComponents, error) {
	var cfg *config.Config
	var err error

	// Get config from watcher if available (it has the validated latest config)
	if configWatcher != nil {
		cfg = configWatcher.GetConfig()
	} else {
		cfg, err = config.LoadConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load config: %w", err)
		}
	}

	// Apply CLI verbose flag
	if cliVerbose || (configWatcher != nil && configWatcher.CLIVerbose()) {
		cfg.Verbose = true
	}

	// systemd/launchd run with minimal PATH, so we add common tool locations
	config.ExpandExecPath(cfg.ExecPath)
	if cfg.Verbose {
		log.Printf("PATH expanded for exec credential plugins")
	}

	manager := tunnelmgr.NewManager(cfg)
	httpServer := httpserver.NewServer(cfg, manager)

	var tcpServer *tcpserver.Server
	if len(cfg.TCP.K8s.Routes) > 0 || len(cfg.TCP.K8s.Jump) > 0 {
		tcpServer = tcpserver.NewServer(cfg, manager)
	}

	return &appComponents{
		cfg:        cfg,
		manager:    manager,
		httpServer: httpServer,
		tcpServer:  tcpServer,
	}, nil
}

func shutdownApp(app *appComponents, ctx context.Context) {
	// HTTP first - stop accepting connections before tearing down tunnels
	if err := app.httpServer.Shutdown(ctx); err != nil {
		log.Printf("Error during HTTP server shutdown: %v", err)
	}

	if app.tcpServer != nil {
		app.tcpServer.Shutdown()
	}

	app.manager.Shutdown()
}

func printConfigInfo(configPath string, cfg *config.Config) {
	fmt.Println("-----------------------------------------------------------------------------")
	if len(cfg.HTTP.K8s.Routes) == 0 && len(cfg.TCP.K8s.Routes) == 0 && len(cfg.TCP.K8s.Jump) == 0 {
		fmt.Println("Add/remove routes !!!❗️⚠️🔴")
	}
	fmt.Printf("Config: %s\n", configPath)
	fmt.Println("-----------------------------------------------------------------------------")
	cfg.PrintRoutes()
	cfg.PrintTCPRoutes()
	cfg.PrintJumpRoutes()
}

// getReloadChan returns the reload channel if watcher exists, or a nil channel that never fires
func getReloadChan(configWatcher *watcher.ConfigWatcher) <-chan struct{} {
	if configWatcher != nil {
		return configWatcher.ReloadChan
	}
	return nil
}
