//go:build linux

package node

import (
	"fmt"
	"syscall"
)

// deviceNumbers reads the major:minor of a block device, which is the only way
// io.max can name a device — the cgroup interface takes numbers, never paths.
//
// The path is stat'ed, not lstat'ed, deliberately: the device the node resolves
// is a /dev/disk/by-id or /dev/mapper name, and both are symlinks onto the sdX,
// nvmeXnY or dm-N node whose numbers the kernel actually accounts against.
//
// The encoding is Linux's own glibc-compatible split: the major is 12 bits at
// offset 8 plus 20 bits at offset 32, and the minor is the 8 low bits plus 12
// bits at offset 12. Both halves matter — a dm device on a busy node can easily
// have a minor above 255, and truncating it would produce a well-formed io.max
// line naming the wrong device, which the kernel accepts and which throttles
// nothing.
func deviceNumbers(path string) (uint32, uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return 0, 0, fmt.Errorf("%s is not a block device", path)
	}
	rdev := uint64(st.Rdev) //nolint:unconvert // Rdev is not uint64 on every Linux arch
	// Each half is masked to 32 bits by construction, so neither conversion can
	// lose information.
	major := uint32(((rdev >> 8) & 0xfff) | ((rdev >> 32) & 0xfffff000))
	minor := uint32((rdev & 0xff) | ((rdev >> 12) & 0xffffff00))
	return major, minor, nil
}
