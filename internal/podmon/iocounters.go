package podmon

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// sample returns a monotonically increasing byte counter for the volume at path.
func (c *ioCounters) sample(path string) (uint64, bool) {
	if dev, ok := c.deviceFor(path); ok {
		if v, ok := c.diskstats(dev); ok {
			return v, true
		}
	}
	return c.mountstats(path)
}

// deviceFor finds the backing block device name for a mounted path.
func (c *ioCounters) deviceFor(path string) (string, bool) {
	b, err := c.readFn(filepath.Join(c.root, "proc", "mounts"))
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || filepath.Clean(unescapeOctal(f[1])) != clean {
			continue
		}
		src := f[0]
		if !strings.HasPrefix(src, "/dev/") {
			return "", false // a network filesystem has no block device
		}
		return strings.TrimPrefix(filepath.Base(src), ""), true
	}
	return "", false
}

// diskstats sums sectors read and written for a device.
func (c *ioCounters) diskstats(device string) (uint64, bool) {
	b, err := c.readFn(filepath.Join(c.root, "proc", "diskstats"))
	if err != nil {
		return 0, false
	}
	s := bufio.NewScanner(strings.NewReader(string(b)))
	for s.Scan() {
		f := strings.Fields(s.Text())
		// major minor name reads merged sectorsRead msReading writes ... sectorsWritten
		if len(f) < 10 || f[2] != device {
			continue
		}
		read, err1 := strconv.ParseUint(f[5], 10, 64)
		written, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, false
		}
		return read + written, true
	}
	return 0, false
}

// mountstats sums bytes read and written for a network mount.
func (c *ioCounters) mountstats(path string) (uint64, bool) {
	b, err := c.readFn(filepath.Join(c.root, "proc", "self", "mountstats"))
	if err != nil {
		return 0, false
	}
	clean := filepath.Clean(path)
	var inMount bool
	s := bufio.NewScanner(strings.NewReader(string(b)))
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "device ") {
			// "device <src> mounted on <target> with fstype <fs> ..."
			f := strings.Fields(line)
			inMount = false
			for i, w := range f {
				if w == "on" && i+1 < len(f) {
					inMount = filepath.Clean(f[i+1]) == clean
					break
				}
			}
			continue
		}
		if !inMount {
			continue
		}
		// "bytes: normalRead normalWrite directRead directWrite serverRead serverWrite ..."
		if strings.HasPrefix(strings.TrimSpace(line), "bytes:") {
			f := strings.Fields(line)
			var total uint64
			for _, v := range f[1:] {
				n, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					continue
				}
				total += n
			}
			return total, true
		}
	}
	return 0, false
}

// unescapeOctal decodes the \040-style escapes /proc/mounts uses in paths.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
