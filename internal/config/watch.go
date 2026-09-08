package config

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchDebounce is how long WatchFile waits for the directory to fall quiet
// before reporting a change.
//
// A Kubernetes projection is several filesystem operations — write the new
// timestamped directory, swap the "..data" symlink, remove the old directory —
// and each one is a separate inotify event. Without a debounce the driver would
// re-read (and re-dial) three or four times for one rotation.
const WatchDebounce = 200 * time.Millisecond

// WatchFile calls onChange whenever the file at path may have changed, until
// ctx ends. It returns only when ctx is done or the watch cannot be
// established.
//
// It watches the file's DIRECTORY, never the file itself. Kubernetes does not
// write into a mounted Secret or ConfigMap: it writes a whole new timestamped
// directory beside the old one, points the hidden "..data" symlink at it with
// an atomic rename, and deletes the previous directory. The visible
// config.yaml is a symlink into "..data" whose own inode is never touched, so
// an inotify watch on the path itself sees no event at all for a rotation —
// and once the target directory is deleted, that watch is pinned to an inode
// nobody will ever write to again. Watching the parent directory is the only
// thing that observes the swap.
//
// onChange is called from a single goroutine, so it does not need to be
// reentrant. It may be called when nothing actually changed; the caller is
// expected to compare the content it reads, not to trust the event.
func WatchFile(ctx context.Context, path string, onChange func()) error {
	dir := filepath.Dir(path)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create file watcher: %w", err)
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}

	// A stopped timer that has never fired: the nil-channel trick below keeps
	// the select from waking up until an event has actually armed it.
	timer := time.NewTimer(WatchDebounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var armed bool
	fire := func() <-chan time.Time {
		if armed {
			return timer.C
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-w.Events:
			if !ok {
				return nil
			}
			// Every event in the directory is interesting: the swap arrives as
			// a Create of "..data" (the rename target), not as a Write of the
			// file the caller named.
			if armed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(WatchDebounce)
			armed = true
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("watching %s: %w", dir, err)
		case <-fire():
			armed = false
			onChange()
		}
	}
}
