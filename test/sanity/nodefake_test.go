package sanity_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// mountingExec stands in for the host's binaries during conformance testing.
//
// It is not a no-op mock: it maintains the fake host's /proc/mounts, so the
// node plugin's own idempotency checks ("is this already mounted?") run against
// state that actually changes. A mock that always succeeded would let the suite
// pass against a node that never tracked anything.
type mountingExec struct {
	root string
	mu   sync.Mutex
}

func newMountingExec(root string) *mountingExec {
	_ = os.MkdirAll(filepath.Join(root, "proc"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "proc", "mounts"), nil, 0o644)
	return &mountingExec{root: root}
}

func (m *mountingExec) mountsPath() string {
	return filepath.Join(m.root, "proc", "mounts")
}

func (m *mountingExec) entries() []string {
	b, err := os.ReadFile(m.mountsPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func (m *mountingExec) write(lines []string) {
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	_ = os.WriteFile(m.mountsPath(), []byte(body), 0o644)
}

func (m *mountingExec) addMount(source, target, fstype string) {
	for _, l := range m.entries() {
		if f := strings.Fields(l); len(f) > 1 && f[1] == target {
			return
		}
	}
	m.write(append(m.entries(),
		fmt.Sprintf("%s %s %s rw,relatime 0 0", source, target, fstype)))
}

func (m *mountingExec) removeMount(target string) {
	var keep []string
	for _, l := range m.entries() {
		if f := strings.Fields(l); len(f) > 1 && f[1] == target {
			continue
		}
		keep = append(keep, l)
	}
	m.write(keep)
}

func (m *mountingExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	base := filepath.Base(name)
	// The node runs everything through nsenter in production.
	if base == "nsenter" {
		for i, a := range args {
			if a == "--" && i+1 < len(args) {
				base = filepath.Base(args[i+1])
				args = args[i+2:]
				break
			}
		}
	}

	switch {
	case base == "mount":
		var positional []string
		fstype := "nfs"
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-t":
				if i+1 < len(args) {
					fstype = args[i+1]
					i++
				}
			case "-o":
				i++
			default:
				if !strings.HasPrefix(args[i], "-") {
					positional = append(positional, args[i])
				}
			}
		}
		if len(positional) >= 2 {
			src, tgt := positional[0], positional[len(positional)-1]
			if err := os.MkdirAll(tgt, 0o750); err != nil && !os.IsExist(err) {
				return nil, err
			}
			m.addMount(src, filepath.Clean(tgt), fstype)
		}
		return nil, nil

	case base == "umount":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				m.removeMount(filepath.Clean(a))
			}
		}
		return nil, nil

	case base == "blkid":
		// No filesystem yet, which is what makes the node run mkfs exactly once.
		return nil, fmt.Errorf("blkid: no filesystem")

	case strings.HasPrefix(base, "mkfs"), base == "resize2fs", base == "xfs_growfs",
		base == "iscsiadm", base == "multipath", base == "modprobe":
		return nil, nil
	}
	return nil, nil
}
