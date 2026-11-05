//go:build darwin

package watcher

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// kqueueNotifier uses kqueue directly. fsnotify's kqueue backend opens every entry of a watched
// directory, which for ~/.autotunnel.yaml is everything in the home directory, including the
// folders macOS protects. A watch on the directory itself already reports any entry being created,
// renamed or removed, so only the directories and the file are opened.
type kqueueNotifier struct {
	kq   int
	wake [2]int // a pipe, written to by close() to end the read loop

	mu  sync.Mutex
	fds map[string]int

	ev   chan struct{}
	errs chan error
	done chan struct{}
}

const vnodeEvents = unix.NOTE_DELETE | unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB |
	unix.NOTE_RENAME | unix.NOTE_REVOKE

func newNotifier() (notifier, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)

	n := &kqueueNotifier{
		kq:   kq,
		fds:  make(map[string]int),
		ev:   make(chan struct{}, 1),
		errs: make(chan error, 1),
		done: make(chan struct{}),
	}
	if err := unix.Pipe(n.wake[:]); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	unix.CloseOnExec(n.wake[0])
	unix.CloseOnExec(n.wake[1])

	var ev unix.Kevent_t
	unix.SetKevent(&ev, n.wake[0], unix.EVFILT_READ, unix.EV_ADD|unix.EV_ENABLE)
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		n.closeFds()
		return nil, err
	}

	go n.read()
	return n, nil
}

func (n *kqueueNotifier) set(dirs []string, file string) error {
	want := make(map[string]bool, len(dirs)+1)
	for _, dir := range dirs {
		want[dir] = true
	}
	if file != "" {
		want[file] = true
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	for path, fd := range n.fds {
		// A replaced file keeps its path, but the old descriptor still watches the old file
		if !want[path] || !samePath(fd, path) {
			_ = unix.Close(fd)
			delete(n.fds, path)
		}
	}

	var errs []error
	for path := range want {
		if _, ok := n.fds[path]; ok {
			continue
		}
		fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
			continue
		}
		var ev unix.Kevent_t
		unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_ENABLE|unix.EV_CLEAR)
		ev.Fflags = vnodeEvents
		if _, err := unix.Kevent(n.kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
			_ = unix.Close(fd)
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		n.fds[path] = fd
	}
	return errors.Join(errs...)
}

// samePath reports whether fd is still the file at path.
func samePath(fd int, path string) bool {
	var open, now unix.Stat_t
	if unix.Fstat(fd, &open) != nil || unix.Stat(path, &now) != nil {
		return false
	}
	return open.Dev == now.Dev && open.Ino == now.Ino
}

func (n *kqueueNotifier) read() {
	defer close(n.done)
	buf := make([]unix.Kevent_t, 16)
	for {
		k, err := unix.Kevent(n.kq, nil, buf, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			select {
			case n.errs <- err:
			default:
			}
			return
		}
		for _, ev := range buf[:k] {
			if int(ev.Ident) == n.wake[0] {
				return
			}
		}
		if k > 0 {
			select {
			case n.ev <- struct{}{}:
			default: // one pending signal is enough
			}
		}
	}
}

func (n *kqueueNotifier) events() <-chan struct{} { return n.ev }
func (n *kqueueNotifier) errors() <-chan error    { return n.errs }

func (n *kqueueNotifier) close() {
	_, _ = unix.Write(n.wake[1], []byte{0})
	<-n.done
	n.closeFds()
}

func (n *kqueueNotifier) closeFds() {
	n.mu.Lock()
	for _, fd := range n.fds {
		_ = unix.Close(fd)
	}
	n.fds = nil
	n.mu.Unlock()
	_ = unix.Close(n.wake[0])
	_ = unix.Close(n.wake[1])
	_ = unix.Close(n.kq)
}
