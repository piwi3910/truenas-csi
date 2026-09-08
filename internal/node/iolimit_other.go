//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package node

import "fmt"

// deviceNumbers has no implementation on a platform with no dev_t worth
// reading. cgroup v2 is a Linux facility and this driver only ever runs on
// Linux; this build exists so the package still compiles everywhere the rest of
// the repository does.
//
// The consequence is a warning at publish and a pod that starts unthrottled,
// never a failed mount — the same as any other reason a limit cannot be applied.
func deviceNumbers(path string) (uint32, uint32, error) {
	return 0, 0, fmt.Errorf("reading the device numbers of %s is not implemented on this platform", path)
}
