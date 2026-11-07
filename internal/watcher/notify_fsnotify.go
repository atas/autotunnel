//go:build !darwin

package watcher

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// fsnotifyNotifier watches directories only. On Linux a directory watch reports writes to its files,
// so the file needs no watch of its own.
type fsnotifyNotifier struct {
	w       *fsnotify.Watcher
	mu      sync.Mutex
	watched map[string]os.FileInfo // the directory each watch was added on
	ev      chan struct{}
	errs    chan error
	done    chan struct{}
}

func newNotifier() (notifier, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &fsnotifyNotifier{
		w:       w,
		watched: make(map[string]os.FileInfo),
		ev:      make(chan struct{}, 1),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
	}
	go n.forward()
	return n, nil
}

func (n *fsnotifyNotifier) set(dirs []string, _ string) error {
	want := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		want[dir] = true
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	for dir, fi := range n.watched {
		// inotify watches the directory, not its path, so one deleted and created again at
		// the same path needs a new watch
		if now, err := os.Stat(dir); !want[dir] || err != nil || !os.SameFile(fi, now) {
			_ = n.w.Remove(dir)
			delete(n.watched, dir)
		}
	}
	var errs []error
	for dir := range want {
		if _, ok := n.watched[dir]; ok {
			continue
		}
		fi, err := os.Stat(dir)
		if err == nil {
			err = n.w.Add(dir)
		}
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			}
			continue
		}
		n.watched[dir] = fi
	}
	return errors.Join(errs...)
}

func (n *fsnotifyNotifier) forward() {
	defer close(n.done)
	for {
		select {
		case event, ok := <-n.w.Events:
			if !ok {
				return
			}
			if event.Op == fsnotify.Chmod {
				continue
			}
			select {
			case n.ev <- struct{}{}:
			default: // one pending signal is enough
			}
		case err, ok := <-n.w.Errors:
			if !ok {
				return
			}
			select {
			case n.errs <- err:
			default:
			}
		}
	}
}

func (n *fsnotifyNotifier) events() <-chan struct{} { return n.ev }
func (n *fsnotifyNotifier) errors() <-chan error    { return n.errs }

func (n *fsnotifyNotifier) close() {
	_ = n.w.Close()
	<-n.done
}
