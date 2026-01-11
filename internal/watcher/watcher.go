package watcher

import (
	"log"
	"sync"
	"time"

	"github.com/atas/lazyfwd/internal/config"
	"github.com/fsnotify/fsnotify"
)

// ConfigUpdater is an interface for updating configuration
type ConfigUpdater interface {
	UpdateConfig(newConfig *config.Config)
}

// ConfigWatcher watches the config file for changes and reloads on valid updates
type ConfigWatcher struct {
	configPath string
	manager    ConfigUpdater
	watcher    *fsnotify.Watcher
	cliVerbose bool // Preserve CLI --verbose flag across reloads

	mu            sync.Mutex
	currentConfig *config.Config

	stopChan chan struct{}
	doneChan chan struct{}
}

// NewConfigWatcher creates a new config file watcher
func NewConfigWatcher(configPath string, initialConfig *config.Config, manager ConfigUpdater, cliVerbose bool) (*ConfigWatcher, error) {
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

			// React to write, create, chmod, or rename events
			// Rename is needed because editors like vim/nano use atomic saves (write temp, rename)
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Chmod|fsnotify.Rename) != 0 {
				// After rename, the old inode is gone - re-watch the file path
				if event.Op&fsnotify.Rename != 0 {
					// Small delay to let the new file appear
					time.Sleep(100 * time.Millisecond)
					// Remove old watch (may fail if already gone, that's ok)
					_ = cw.watcher.Remove(cw.configPath)
					// Add watch for the new file at this path
					_ = cw.watcher.Add(cw.configPath)
				}

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

	// LoadConfig now includes validation
	newConfig, err := config.LoadConfig(cw.configPath)
	if err != nil {
		log.Printf("Failed to load config: %v (keeping current config)", err)
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
