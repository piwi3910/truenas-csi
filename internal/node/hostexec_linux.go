//go:build linux

package node

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// run executes a host binary with the host filesystem as its root.
//
// The driver image is a static base with no shell and no nsenter, so shelling
// out to nsenter fails outright. Chrooting the child into the mounted host root
// runs the host's own binaries instead, needing nothing in the image. Mounts
// made this way reach the host because the kubelet directory is mounted with
// Bidirectional propagation.
func (h hostExec) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := resolveHostBinary(h.root, name)
	if err != nil {
		return nil, err
	}
	// The command name never comes from user input: callers pass fixed
	// literals ("mount", "iscsiadm", "mkfs.ext4"), and resolveHostBinary
	// accepts only a name found in a fixed list of host directories.
	// nosemgrep: dangerous-exec-command
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Path = path // do not let Go re-resolve it against the container's PATH
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: h.root}
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=" + strings.Join(hostBinDirs, ":")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
