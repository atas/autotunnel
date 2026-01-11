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

	"github.com/atas/lazyfwd/internal"
	"github.com/atas/lazyfwd/internal/config"
	"github.com/atas/lazyfwd/internal/httpserver"
	"github.com/atas/lazyfwd/internal/tunnelmgr"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Parse flags
	var configPath string
	var verbose bool
	var showVersion bool

	home, _ := os.UserHomeDir()
	defaultConfig := filepath.Join(home, ".lazyfwd.yaml")

	flag.StringVar(&configPath, "config", defaultConfig, "Path to configuration file")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.Parse()

	if showVersion {
		fmt.Printf("lazyfwd version %s (commit: %s, built: %s)\n", version, commit, date)
		return
	}

	// Print banner
	fmt.Println(`
██╗      █████╗ ███████╗██╗   ██╗███████╗██╗    ██╗██████╗  ██╗    ██╗
██║     ██╔══██╗╚══███╔╝╚██╗ ██╔╝██╔════╝██║    ██║██╔══██╗ ╚██╗   ╚██╗
██║     ███████║  ███╔╝  ╚████╔╝ █████╗  ██║ █╗ ██║██║  ██║  ╚██╗   ╚██╗
██║     ██╔══██║ ███╔╝    ╚██╔╝  ██╔══╝  ██║███╗██║██║  ██║  ██╔╝   ██╔╝
███████╗██║  ██║███████╗   ██║   ██║     ╚███╔███╔╝██████╔╝ ██╔╝   ██╔╝
╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝   ╚═╝      ╚══╝╚══╝ ╚═════╝ ╚═╝    ╚═╝

On-demand Port Forwarding
https://github.com/atas/lazyfwd`)

	// Configure logging
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("[lazyfwd] ")

	// Check if config exists, create default if not
	if !config.ConfigExists(configPath) {
		fmt.Printf("Config file not found: %s\n", configPath)
		fmt.Println("Creating default configuration file...")

		if err := config.CreateDefaultConfig(configPath); err != nil {
			log.Fatalf("Failed to create config file: %v", err)
		}

		fmt.Printf("\nCreated: %s\n", configPath)
	}

	// Load configuration (includes validation)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configPath, err)
	}

	// Override verbose from flag
	if verbose {
		cfg.Verbose = true
	}

	fmt.Println("-----------------------------------------------------------------------------")
	if len(cfg.HTTP.K8s.Routes) == 0 {
		fmt.Println("Add/remove routes !!!❗️⚠️🔴")
	}
	fmt.Printf("Config: %s\n", configPath)
	fmt.Println("-----------------------------------------------------------------------------")
	cfg.LogRoutes()

	// Create manager and server
	manager := tunnelmgr.NewManager(cfg)
	server := httpserver.NewServer(cfg, manager)

	// Start manager
	manager.Start()

	// Start config watcher if auto-reload is enabled
	var configWatcher *internal.ConfigWatcher
	if cfg.ShouldAutoReload() {
		var err error
		configWatcher, err = internal.NewConfigWatcher(configPath, cfg, manager, verbose)
		if err != nil {
			log.Printf("Warning: Failed to start config watcher: %v", err)
		} else {
			configWatcher.Start()
		}
	}

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start unified server in goroutine
	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	fmt.Printf("Listening on %s\n", cfg.HTTP.ListenAddr)

	// Wait for signal
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop config watcher
	if configWatcher != nil {
		configWatcher.Stop()
	}

	// Shutdown server first (stop accepting new connections)
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error during server shutdown: %v", err)
	}

	// Then shutdown manager (close all tunnels)
	manager.Shutdown()

	log.Println("Shutdown complete")
}
