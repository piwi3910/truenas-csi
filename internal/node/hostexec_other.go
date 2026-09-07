//go:build !linux

package node

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// run executes the binary directly. Chrooting is a Linux facility, and the node
// plugin only ever runs on Linux; this build exists so the package still
// compiles and unit-tests on a developer's machine.
func (h hostExec) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	// The command name never comes from user input: callers pass fixed
	// literals ("mount", "iscsiadm", "mkfs.ext4"), and resolveHostBinary
	// accepts only a name found in a fixed list of host directories.
	// nosemgrep: dangerous-exec-command
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
