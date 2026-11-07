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
	if verbose {
		cfg.Verbose = true
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

	if err := run(configPath, cfg, configWatcher, sigChan); err != nil {
		log.Fatalf("Failed to start: %v", err)
	}

	log.Println("Shutdown complete")
}

// run serves the config until a signal arrives, restarting whenever the watcher reports a change.
// It returns an error only when a config cannot start and there is no earlier one to go back to.
func run(configPath string, cfg *config.Config, configWatcher *watcher.ConfigWatcher, sigChan <-chan os.Signal) error {
	// The config to go back to if the next start fails. Nil at first start and right after going back.
	var fallback *config.Config

	for {
		if configWatcher != nil {
			cfg = configWatcher.GetConfig()
		}
		app := initializeApp(cfg)

		printConfigInfo(configPath, app.cfg)

		// Start servers
		app.manager.Start()

		serverErrChan := make(chan error, 1)

		// TCP binds before Start returns, so HTTP is not started just to be shut down again
		if app.tcpServer != nil {
			if err := app.tcpServer.Start(); err != nil {
				serverErrChan <- fmt.Errorf("TCP server: %w", err)
			}
		}
		if len(serverErrChan) == 0 {
			go func() {
				if err := app.httpServer.Start(); err != nil {
					serverErrChan <- err
				}
			}()
		}

		// Wait for signal or config reload
		shouldExit := false
		var startErr error
		select {
		case sig := <-sigChan:
			log.Printf("Received signal %v, shutting down...", sig)
			shouldExit = true

		case startErr = <-serverErrChan:

		case <-getReloadChan(configWatcher):
			log.Println("Config changed, restarting...")
			if len(serverErrChan) == 0 {
				fallback = app.cfg
			}
		}

		// Shutdown current instance
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		shutdownApp(app, ctx)
		cancel()

		if shouldExit {
			return nil
		}

		if startErr != nil {
			if fallback == nil {
				return startErr
			}
			if configWatcher.RestoreConfig(app.cfg, fallback) {
				log.Printf("Failed to start with new config: %v (restoring previous config)", startErr)
				fallback = nil
			} else {
				// A newer config came in while this one failed: try it, and keep fallback for it
				log.Printf("Failed to start with new config: %v (trying the newer config)", startErr)
			}
		}

		// Small delay before restart to let ports release
		time.Sleep(100 * time.Millisecond)
	}
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

func initializeApp(cfg *config.Config) *appComponents {
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
	}
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
