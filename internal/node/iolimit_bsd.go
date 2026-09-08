//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package node

import (
	"fmt"
	"syscall"
)

// deviceNumbers reads the major:minor of a block device.
//
// The BSDs — darwin among them — split dev_t as an 8-bit major at offset 24 and
// a 24-bit minor, which is not Linux's split. This build exists because the
// repository builds and unit-tests on darwin; the CGROUP half of I/O limits is
// Linux-only and fails there of its own accord, since /sys/fs/cgroup does not
// exist. Getting the numbers right anyway keeps this function honest on every
// platform it compiles for, rather than being a stub that always fails.
func deviceNumbers(path string) (uint32, uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return 0, 0, fmt.Errorf("%s is not a block device", path)
	}
	rdev := uint32(st.Rdev) //nolint:gosec // dev_t is 32 bits wide on these platforms
	return (rdev >> 24) & 0xff, rdev & 0xffffff, nil
}
