package node

// Mount plumbing. Everything here is written to be idempotent, because the kubelet
// retries every node RPC: a mount that already exists must not be made twice, and an
// unmount of something absent must succeed rather than wedge a volume's teardown.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// mountEntry is one line of the host's mount table.
type mountEntry struct {
	source string
	target string
	fsType string
	opts   string
}

// mounts reads the host's mount table through the mounted host root. A missing table
// means the host procfs is not visible to this container; that is reported as "no
// mounts" rather than an error, so a misconfigured mount does not turn every
// idempotency check into a hard failure — the worst case is a duplicate mount
// attempt, which mount(8) itself rejects.
func (n *Node) mounts() ([]mountEntry, error) {
	b, err := os.ReadFile(filepath.Join(n.hostRoot(), "proc", "mounts"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read host mount table: %w", err)
	}
	var out []mountEntry
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		out = append(out, mountEntry{
			// /proc/mounts octal-escapes spaces and tabs in paths.
			source: unescapeMountField(f[0]),
			target: unescapeMountField(f[1]),
			fsType: f[2],
			opts:   f[3],
		})
	}
	return out, nil
}

// hostRoot is Root with the production default applied.
func (n *Node) hostRoot() string {
	if n.Root == "" {
		return DefaultHostRoot
	}
	return n.Root
}

// isMounted reports whether path is a mount point on the host.
func (n *Node) isMounted(path string) (bool, error) {
	entries, err := n.mounts()
	if err != nil {
		return false, err
	}
	clean := filepath.Clean(path)
	for _, e := range entries {
		if e.target == clean {
			return true, nil
		}
	}
	return false, nil
}

// unescapeMountField decodes the \040-style octal escapes /proc/mounts uses for
// characters that would otherwise split a field.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mount runs mount(8) on the host with an explicit type and option list.
func (n *Node) mount(ctx context.Context, fsType, source, target string, opts []string) error {
	args := make([]string, 0, 7)
	if fsType != "" {
		args = append(args, "-t", fsType)
	}
	if len(opts) > 0 {
		args = append(args, "-o", strings.Join(opts, ","))
	}
	args = append(args, source, target)
	if _, err := n.exec.Run(ctx, "mount", args...); err != nil {
		return fmt.Errorf("mount %s at %s: %w", source, target, err)
	}
	return nil
}

// bindMount binds source onto target. A read-only bind needs two calls: Linux
// ignores "ro" on the initial bind and only honours it on a subsequent
// remount,bind,ro, so issuing one command would silently produce a writable mount.
func (n *Node) bindMount(ctx context.Context, source, target string, readonly bool) error {
	if _, err := n.exec.Run(ctx, "mount", "-o", "bind", source, target); err != nil {
		return fmt.Errorf("bind mount %s at %s: %w", source, target, err)
	}
	if !readonly {
		return nil
	}
	if _, err := n.exec.Run(ctx, "mount", "-o", "remount,bind,ro", target); err != nil {
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

// unmountIfMounted unmounts path when the host reports it as a mount point, and
// otherwise does nothing at all. The "nothing at all" is the point: a repeated
// NodeUnstageVolume must not issue a umount that fails.
func (n *Node) unmountIfMounted(ctx context.Context, path string) error {
	mounted, err := n.isMounted(path)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if _, err := n.exec.Run(ctx, "umount", path); err != nil {
		return fmt.Errorf("umount %s: %w", path, err)
	}
	return nil
}
