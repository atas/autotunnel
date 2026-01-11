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
  _                     __          __
 | |                   / _|        / _|
 | |     __ _ _____   | |___      | |_ __      __ __
 | |    / _' |_  / |  |  _\ \ /\ / / _' |
 | |___| (_| |/ /| |_| | |  \ V  V | (_| |
 |______\__,_/___|___, |_|   \_/\_/ \__,_|
                   __/ |
                  |___/
  On-demand Port Forwarding
  https://github.com/atas/lazyfwd`)

	// Configure logging
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("[lazyfwd] ")

	// Check if config exists, create default if not
	if !internal.ConfigExists(configPath) {
		fmt.Printf("Config file not found: %s\n", configPath)
		fmt.Println("Creating default configuration file...")

		if err := internal.CreateDefaultConfig(configPath); err != nil {
			log.Fatalf("Failed to create config file: %v", err)
		}

		fmt.Printf("\nCreated: %s\n", configPath)
		fmt.Println("\nNext steps:")
		fmt.Println("  1. Edit the config file to add your Kubernetes services")
		fmt.Println("  2. Run 'lazyfwd' again to start the proxy")
		fmt.Println("\nExample service configuration:")
		fmt.Println("  http:")
		fmt.Println("    k8s:")
		fmt.Println("      services:")
		fmt.Println("        grafana.localhost:")
		fmt.Println("          context: my-cluster")
		fmt.Println("          namespace: monitoring")
		fmt.Println("          service: grafana")
		fmt.Println("          port: 3000")
		return
	}

	// Load configuration
	config, err := internal.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configPath, err)
	}

	// Override verbose from flag
	if verbose {
		config.Verbose = true
	}

	if err := config.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	fmt.Printf("Config: %s (edit to add/remove routes)\n", configPath)
	config.LogRoutes()

	// Create manager and server
	manager := internal.NewManager(config)
	server := internal.NewServer(config, manager)

	// Start manager
	manager.Start()

	// Start config watcher if auto-reload is enabled
	var configWatcher *internal.ConfigWatcher
	if config.ShouldAutoReload() {
		var err error
		configWatcher, err = internal.NewConfigWatcher(configPath, config, manager, verbose)
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

	fmt.Printf("Ready! Listening on %s\n", config.HTTP.ListenAddr)
	fmt.Println("Press Ctrl+C to stop")

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
