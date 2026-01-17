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

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configPath, err)
	}

	if verbose {
		cfg.Verbose = true
	}

	// systemd/launchd run with minimal PATH, so we add common tool locations
	config.ExpandExecPath(cfg.ExecPath)
	if cfg.Verbose {
		log.Printf("PATH expanded for exec credential plugins")
	}

	fmt.Println("-----------------------------------------------------------------------------")
	if len(cfg.HTTP.K8s.Routes) == 0 && len(cfg.TCP.K8s.Routes) == 0 && len(cfg.TCP.K8s.Socat) == 0 {
		fmt.Println("Add/remove routes !!!❗️⚠️🔴")
	}
	fmt.Printf("Config: %s\n", configPath)
	fmt.Println("-----------------------------------------------------------------------------")
	cfg.PrintRoutes()
	cfg.PrintTCPRoutes()
	cfg.PrintSocatRoutes()

	manager := tunnelmgr.NewManager(cfg)
	server := httpserver.NewServer(cfg, manager)

	var tcpServer *tcpserver.Server
	if len(cfg.TCP.K8s.Routes) > 0 || len(cfg.TCP.K8s.Socat) > 0 {
		tcpServer = tcpserver.NewServer(cfg, manager)
	}

	manager.Start()

	var configWatcher *watcher.ConfigWatcher
	if cfg.ShouldAutoReload() {
		var err error
		configWatcher, err = watcher.NewConfigWatcher(configPath, cfg, manager, verbose)
		if err != nil {
			log.Printf("Warning: Failed to start config watcher: %v", err)
		} else {
			configWatcher.Start()
			// Register TCP server with the config watcher
			if tcpServer != nil {
				configWatcher.SetTCPServer(tcpServer)
			}
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	if tcpServer != nil {
		if err := tcpServer.Start(); err != nil {
			log.Fatalf("Failed to start TCP server: %v", err)
		}
	}

	fmt.Printf("Listening on %s\n", cfg.HTTP.ListenAddr)

	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	gracefulShutdown(server, tcpServer, manager, configWatcher)
}

func gracefulShutdown(server *httpserver.Server, tcpServer *tcpserver.Server, manager *tunnelmgr.Manager, configWatcher *watcher.ConfigWatcher) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if configWatcher != nil {
		configWatcher.Stop()
	}

	// HTTP first - stop accepting connections before tearing down tunnels
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error during server shutdown: %v", err)
	}

	if tcpServer != nil {
		tcpServer.Shutdown()
	}

	manager.Shutdown()

	log.Println("Shutdown complete")
}
