package podmon

import (
	"os"
	"sync"
	"time"
)

// ioCounters answers "has this volume done any I/O lately" from kernel counters
// that actually advance on reads and writes.
//
// The obvious proxy — the mount point's mtime — does not work, and was verified
// not to work against a real NFS mount: 4 MiB written to an existing file left
// the directory's mtime untouched, because a directory's mtime tracks entries
// being created and removed, not writes to files already in it. A database
// steadily writing its data file would have reported no I/O at all, which is
// exactly the case this answer exists for.
//
// Instead:
//   - block volumes (iSCSI, NVMe-oF) are read from /proc/diskstats, whose
//     completed-sector fields advance with every read and write;
//   - network filesystems (NFS, SMB) are read from /proc/self/mountstats, whose
//     per-mount byte counters advance the same way.
//
// The parsing lives in volumeio.go, which the exported per-volume metrics share:
// one reader, so a fix to either answer fixes both. This one reduces the whole
// reading to a single number and watches it move — activity() sums every
// counter, page-cache traffic included, because a read served without touching
// the NAS is still the pod doing I/O.
//
// Both are counters, so a single sample says nothing. The reader keeps the last
// sample per path and reports I/O when the counter has moved since.
type ioCounters struct {
	root string

	mu     sync.Mutex
	last   map[string]counterSample
	nowFn  func() time.Time
	readFn func(string) ([]byte, error)
}

type counterSample struct {
	value uint64
	at    time.Time
}

func newIOCounters(root string) *ioCounters {
	return &ioCounters{
		root:   root,
		last:   map[string]counterSample{},
		nowFn:  time.Now,
		readFn: os.ReadFile,
	}
}

// Active reports whether the volume mounted at path has moved any bytes since
// the previous call, and when that was last observed.
//
// The first call for a path can only record a baseline, so it reports no
// activity rather than guessing. That is the safe direction: claiming I/O that
// did not happen would keep a dead pod alive.
func (c *ioCounters) Active(path string) (time.Time, bool) {
	v, ok := c.sample(path)
	if !ok {
		return time.Time{}, false
	}
	now := c.nowFn()

	c.mu.Lock()
	defer c.mu.Unlock()
	prev, seen := c.last[path]
	c.last[path] = counterSample{value: v, at: now}
	if !seen {
		return time.Time{}, false
	}
	if v != prev.value {
		return now, true
	}
	// Unchanged: the last time it genuinely moved is the best answer available.
	return prev.at, true
}

// sample returns a monotonically increasing activity counter for the volume at
// path.
//
// The reader is built per call rather than held, so that a test which replaces
// readFn after construction — as the probe's own regression tests do — still
// reaches the parsing underneath.
func (c *ioCounters) sample(path string) (uint64, bool) {
	stats := &IOStats{root: c.root, readFile: c.readFn, readLink: os.Readlink}
	v, ok := stats.read(path, "")
	if !ok {
		return 0, false
	}
	return v.activity(), true
}
