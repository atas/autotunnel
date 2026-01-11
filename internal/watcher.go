package internal

import (
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ConfigWatcher watches the config file for changes and reloads on valid updates
type ConfigWatcher struct {
	configPath string
	manager    *Manager
	watcher    *fsnotify.Watcher
	cliVerbose bool // Preserve CLI --verbose flag across reloads

	mu            sync.Mutex
	currentConfig *Config

	stopChan chan struct{}
	doneChan chan struct{}
}

// NewConfigWatcher creates a new config file watcher
func NewConfigWatcher(configPath string, initialConfig *Config, manager *Manager, cliVerbose bool) (*ConfigWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := watcher.Add(configPath); err != nil {
		watcher.Close()
		return nil, err
	}

	return &ConfigWatcher{
		configPath:    configPath,
		manager:       manager,
		watcher:       watcher,
		cliVerbose:    cliVerbose,
		currentConfig: initialConfig,
		stopChan:      make(chan struct{}),
		doneChan:      make(chan struct{}),
	}, nil
}

// Start begins watching the config file for changes
func (cw *ConfigWatcher) Start() {
	go cw.watchLoop()
}

// Stop stops the config watcher
func (cw *ConfigWatcher) Stop() {
	close(cw.stopChan)
	cw.watcher.Close()
	<-cw.doneChan
}

func (cw *ConfigWatcher) watchLoop() {
	defer close(cw.doneChan)

	var debounceTimer *time.Timer
	var debounceMu sync.Mutex

	for {
		select {
		case <-cw.stopChan:
			debounceMu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceMu.Unlock()
			return

		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			// React to write, create, or chmod events
			// Create handles vim/editor atomic saves (write to temp, rename)
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Chmod) != 0 {
				debounceMu.Lock()
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
					cw.reloadConfig()
				})
				debounceMu.Unlock()
			}

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}

func (cw *ConfigWatcher) reloadConfig() {
	log.Println("Config file changed, reloading...")

	newConfig, err := LoadConfig(cw.configPath)
	if err != nil {
		log.Printf("Failed to load config: %v (keeping current config)", err)
		return
	}

	if err := newConfig.Validate(); err != nil {
		log.Printf("Invalid config: %v (keeping current config)", err)
		return
	}

	// Preserve CLI verbose flag
	if cw.cliVerbose {
		newConfig.Verbose = true
	}

	cw.mu.Lock()
	cw.currentConfig = newConfig
	cw.mu.Unlock()

	cw.manager.UpdateConfig(newConfig)

	log.Println("Config reloaded successfully")
	newConfig.LogRoutes()
}
