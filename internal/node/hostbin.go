package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostBinDirs are searched, under the host root, for the binaries the node
// plugin drives.
var hostBinDirs = []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin", "/usr/local/sbin", "/usr/local/bin"}

// resolveHostBinary finds a binary inside the mounted host root and returns the
// path it will have AFTER the chroot.
//
// Go resolves a command's path in the parent process, before SysProcAttr.Chroot
// takes effect, so a bare name like "mount" is looked up in the container's own
// PATH and fails with "executable file not found" — as it did on a real
// cluster, even though every node had /usr/bin/mount. Resolving against the
// host root and setting Cmd.Path explicitly makes the lookup and the execution
// agree about which filesystem they are talking about.
func resolveHostBinary(root, name string) (string, error) {
	if root == "" {
		root = DefaultHostRoot
	}
	if strings.Contains(name, "/") {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			return "", fmt.Errorf("%s not present on the host: %w", name, err)
		}
		return name, nil
	}
	for _, dir := range hostBinDirs {
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(filepath.Join(root, candidate)); err == nil && !fi.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found on the host in %s", name, strings.Join(hostBinDirs, ", "))
}
