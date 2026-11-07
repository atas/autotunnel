package watcher

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atas/autotunnel/internal/config"
)

// maxLinks bounds how many symlinks are followed from the config path, like the kernel's limit.
const maxLinks = 40

// notifier reports that something changed in the watched directories or file. It does not say what:
// the watcher compares a snapshot of the config instead, so every backend behaves the same.
type notifier interface {
	// set replaces what is watched. A path that does not exist yet is skipped without an error.
	set(dirs []string, file string) error
	events() <-chan struct{}
	errors() <-chan error
	close()
}

type ConfigWatcher struct {
	configPath string
	watchPath  string // configPath made absolute, with its directory's symlinks resolved
	notifier   notifier
	cliVerbose bool // Preserve CLI --verbose flag across reloads

	mu            sync.Mutex
	currentConfig *config.Config
	lastSnapshot  string // what the config looked like when it was last loaded

	// ReloadChan signals that config has changed and app should restart
	// Buffered with capacity 1 to avoid blocking
	ReloadChan chan struct{}

	stopChan chan struct{}
	doneChan chan struct{}

	lastWatchErr string // logged once, not again on every event
}

func NewConfigWatcher(configPath string, initialConfig *config.Config, cliVerbose bool) (*ConfigWatcher, error) {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return nil, err
	}

	n, err := newNotifier()
	if err != nil {
		return nil, err
	}

	cw := &ConfigWatcher{
		configPath:    configPath,
		watchPath:     filepath.Join(dir, filepath.Base(absPath)),
		notifier:      n,
		cliVerbose:    cliVerbose,
		currentConfig: initialConfig,
		ReloadChan:    make(chan struct{}, 1),
		stopChan:      make(chan struct{}),
		doneChan:      make(chan struct{}),
	}

	// Watching only the file would lose it when a save replaces the file, so its directory is watched
	// too, and the directory of every symlink on the way to it
	// A directory that can't be watched (no read permission, or macOS privacy protection) is
	// logged and skipped, and the others are still watched
	chain := resolveChain(cw.watchPath)
	cw.logWatchError(n.set(chain.dirs, chain.file))
	cw.lastSnapshot = chain.snapshot()
	return cw, nil
}

// logWatchError logs err from setting up the watches, unless it is the one logged last time.
func (cw *ConfigWatcher) logWatchError(err error) {
	msg := ""
	if err != nil {
		// set joins one error per path in map order, so sort them to compare
		lines := strings.Split(err.Error(), "\n")
		sort.Strings(lines)
		msg = strings.Join(lines, "\n")
	}
	if msg != "" && msg != cw.lastWatchErr {
		log.Printf("Config watcher: can't watch %s (changes there won't trigger a reload)", msg)
	}
	cw.lastWatchErr = msg
}

func (cw *ConfigWatcher) Start() {
	go cw.watchLoop()
}

func (cw *ConfigWatcher) Stop() {
	close(cw.stopChan)
	cw.notifier.close()
	<-cw.doneChan
}

func (cw *ConfigWatcher) watchLoop() {
	defer close(cw.doneChan)

	var debounceTimer *time.Timer
	var debounceMu sync.Mutex

	// The snapshot at the last event that changed it. Events for other files in the directory leave
	// it as it is, so they cannot keep pushing the reload back.
	cw.mu.Lock()
	seen := cw.lastSnapshot
	cw.mu.Unlock()

	scheduleReload := func() {
		debounceMu.Lock()
		defer debounceMu.Unlock()
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(500*time.Millisecond, cw.reloadIfChanged)
	}

	for {
		select {
		case <-cw.stopChan:
			debounceMu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceMu.Unlock()
			return

		case <-cw.notifier.events():
			// Follow a replaced file or a moved symlink at once, so the next edit is seen too
			chain := resolveChain(cw.watchPath)
			cw.logWatchError(cw.notifier.set(chain.dirs, chain.file))
			if snapshot := chain.snapshot(); snapshot != seen {
				seen = snapshot
				scheduleReload()
			}

		case err := <-cw.notifier.errors():
			log.Printf("Config watcher error: %v", err)
		}
	}
}

// reloadIfChanged reloads unless the config's file and symlinks look as they did at the last load.
// Events also come for other files in the watched directories, and for chmod.
func (cw *ConfigWatcher) reloadIfChanged() {
	snapshot := resolveChain(cw.watchPath).snapshot()
	cw.mu.Lock()
	changed := snapshot != cw.lastSnapshot
	cw.lastSnapshot = snapshot
	cw.mu.Unlock()
	if changed {
		cw.reloadConfig()
	}
}

// chain is the way from the config path to the file it ends at.
type chain struct {
	links []string // each symlink followed, and where it pointed
	dirs  []string // the directory of every link and of the file, with their symlinks resolved
	file  string
}

// resolveChain follows the symlinks from path one at a time, as a change to any of them, in any
// directory, changes the config. A symlinked directory along the way is resolved, not watched.
func resolveChain(path string) chain {
	var c chain
	seen := make(map[string]bool)
	for i := 0; ; i++ {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(dir, filepath.Base(path))
		}
		if dir := filepath.Dir(path); !seen[dir] {
			seen[dir] = true
			c.dirs = append(c.dirs, dir)
			// A missing directory can't be watched, so its nearest existing parent is, to see it
			// come back. Missing ones in between are skipped by the notifier.
			for d := dir; !exists(d) && filepath.Dir(d) != d; {
				d = filepath.Dir(d)
				if !seen[d] {
					seen[d] = true
					c.dirs = append(c.dirs, d)
				}
			}
		}

		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 || i == maxLinks {
			c.file = path
			return c
		}
		target, err := os.Readlink(path)
		if err != nil {
			c.file = path
			return c
		}
		c.links = append(c.links, path+" -> "+target)
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapshot changes when the config does: a symlink moved, or the file was written, replaced or removed.
// The mode is left out, as chmod says nothing about the content.
func (c chain) snapshot() string {
	s := fmt.Sprint(c.links, c.file)
	if fi, err := os.Stat(c.file); err == nil {
		s += fmt.Sprintf(" %d %d", fi.Size(), fi.ModTime().UnixNano())
	} else {
		s += " missing"
	}
	return s
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

	current := cw.GetConfig()
	if reflect.DeepEqual(current, newConfig) {
		log.Println("Config unchanged, nothing to do")
		return
	}

	if err := checkNewPorts(current, newConfig); err != nil {
		log.Printf("Cannot apply new config: %v (keeping current config)", err)
		return
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

// checkNewPorts tries to bind what next needs on top of current, while current still runs. Ports
// current holds are skipped, as they are busy because of us. main.go handles a failure on those.
func checkNewPorts(current, next *config.Config) error {
	held := make(map[string]bool)
	if _, port, err := net.SplitHostPort(current.HTTP.ListenAddr); err == nil {
		held[port] = true
	}
	for _, port := range tcpPorts(current) {
		held[strconv.Itoa(port)] = true
	}

	var addrs []string
	if next.HTTP.ListenAddr != current.HTTP.ListenAddr {
		if _, port, err := net.SplitHostPort(next.HTTP.ListenAddr); err != nil || !held[port] {
			addrs = append(addrs, next.HTTP.ListenAddr)
		}
	}
	for _, port := range tcpPorts(next) {
		if !held[strconv.Itoa(port)] {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port)) // as tcpserver binds it
		}
	}

	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		_ = l.Close()
	}
	return nil
}

func tcpPorts(cfg *config.Config) []int {
	ports := make([]int, 0, len(cfg.TCP.K8s.Routes)+len(cfg.TCP.K8s.Jump))
	for port := range cfg.TCP.K8s.Routes {
		ports = append(ports, port)
	}
	for port := range cfg.TCP.K8s.Jump {
		ports = append(ports, port)
	}
	return ports
}

// GetConfig returns the current validated config
func (cw *ConfigWatcher) GetConfig() *config.Config {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.currentConfig
}

// RestoreConfig puts previous back after failed, the config the watcher handed out, did not start.
// Saving the failed file again then counts as a change, so it is retried. If the watcher has
// accepted a newer config meanwhile, that one stays and RestoreConfig returns false.
func (cw *ConfigWatcher) RestoreConfig(failed, previous *config.Config) bool {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.currentConfig != failed {
		return false
	}
	cw.currentConfig = previous
	return true
}
