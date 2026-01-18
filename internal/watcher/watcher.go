package watcher

import (
	"log"
	"sync"
	"time"

	"github.com/atas/autotunnel/internal/config"
	"github.com/fsnotify/fsnotify"
)

type ConfigWatcher struct {
	configPath string
	watcher    *fsnotify.Watcher
	cliVerbose bool // Preserve CLI --verbose flag across reloads

	mu            sync.Mutex
	currentConfig *config.Config

	// ReloadChan signals that config has changed and app should restart
	// Buffered with capacity 1 to avoid blocking
	ReloadChan chan struct{}

	stopChan chan struct{}
	doneChan chan struct{}
}

func NewConfigWatcher(configPath string, initialConfig *config.Config, cliVerbose bool) (*ConfigWatcher, error) {
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
		watcher:       watcher,
		cliVerbose:    cliVerbose,
		currentConfig: initialConfig,
		ReloadChan:    make(chan struct{}, 1),
		stopChan:      make(chan struct{}),
		doneChan:      make(chan struct{}),
	}, nil
}

func (cw *ConfigWatcher) Start() {
	go cw.watchLoop()
}

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

			// vim/nano do atomic saves (write temp, rename), so we need to watch for Rename too
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Chmod|fsnotify.Rename) != 0 {
				// after rename, old inode is gone so we need to re-watch the path
				if event.Op&fsnotify.Rename != 0 {
					time.Sleep(100 * time.Millisecond) // let new file appear
					_ = cw.watcher.Remove(cw.configPath)
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
	log.Println("Config file changed, validating...")

	newConfig, err := config.LoadConfig(cw.configPath)
	if err != nil {
		log.Printf("Failed to load config: %v (keeping current config)", err)
		return
	}

	if cw.cliVerbose {
		newConfig.Verbose = true
	}

	cw.mu.Lock()
	cw.currentConfig = newConfig
	cw.mu.Unlock()

	// Signal main.go to restart the application
	// Non-blocking send (channel has capacity 1)
	select {
	case cw.ReloadChan <- struct{}{}:
		log.Println("Config changed, triggering restart...")
	default:
		// Already a reload pending, skip
	}
}

// GetConfig returns the current validated config
func (cw *ConfigWatcher) GetConfig() *config.Config {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.currentConfig
}

// CLIVerbose returns the CLI verbose flag setting
func (cw *ConfigWatcher) CLIVerbose() bool {
	return cw.cliVerbose
}
