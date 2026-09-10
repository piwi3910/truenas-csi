// Package node implements the CSI node plugin. This file holds the capability
// preflight: the node plugin ships no storage tooling of its own, so at startup it
// probes the host filesystem for the binaries and kernel modules each protocol needs,
// advertises only what it can actually deliver, and names the package an operator must
// install for anything missing.
package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Capability is one thing this node can do: speak a protocol, or handle a filesystem.
type Capability string

const (
	// CapNFS is the ability to mount NFS exports.
	CapNFS Capability = "nfs"
	// CapISCSI is the ability to attach iSCSI targets.
	CapISCSI Capability = "iscsi"
	// CapExt4 is the ability to create and grow ext4 filesystems.
	CapExt4 Capability = "ext4"
	// CapXFS is the ability to create and grow XFS filesystems.
	CapXFS Capability = "xfs"
	// CapNVMe is the ability to attach NVMe-oF namespaces.
	CapNVMe Capability = "nvme"
	// CapSMB is the ability to mount SMB shares with the kernel's cifs client.
	// Its string value is the protocol name, not the filesystem name, because
	// the controller derives the topology requirement for a volume straight from
	// its StorageClass protocol.
	CapSMB Capability = "smb"
	// CapMultipath is the ability to use device-mapper multipath for iSCSI.
	CapMultipath Capability = "multipath"
)

// ErrCapabilityUnavailable is the sentinel every Require failure wraps, so callers can
// map the condition onto a gRPC code without matching on message text.
var ErrCapabilityUnavailable = errors.New("node lacks the tooling this volume requires")

// binDirs are the directories under the host root searched for binaries, in order.
// They are searched by absolute path rather than by consulting PATH: the node plugin's
// own PATH describes its container, not the host.
var binDirs = []string{"sbin", "usr/sbin", "bin", "usr/bin"}

// requirement is what one capability needs from the host.
type requirement struct {
	// bins are binary names that must all be present under one of binDirs.
	bins []string
	// mods are kernel modules that must be loaded, or loadable from disk.
	mods []string
	// pkg is the distribution package providing the missing pieces, named in the
	// error an operator sees.
	pkg string
}

// requirements is the measured table of what each capability costs on a Debian-family
// host. Module names use the /proc/modules spelling (underscores); the .ko search also
// tries the hyphenated file name.
var requirements = map[Capability]requirement{
	CapNFS:   {bins: []string{"mount.nfs"}, pkg: "nfs-common"},
	CapISCSI: {bins: []string{"iscsiadm", "iscsid"}, mods: []string{"iscsi_tcp"}, pkg: "open-iscsi"},
	// blkid is listed with both filesystems deliberately. It is what decides
	// whether a staged device is blank, and it is the ONLY thing standing
	// between a retried NodeStageVolume and mkfs over somebody's data. A node
	// missing it used to advertise these capabilities anyway and refuse every
	// stage at mount time; refusing the capability instead says why, once,
	// before any volume is scheduled here. It ships in util-linux, so it is
	// present on any host that has the rest of this table.
	CapExt4: {bins: []string{"mkfs.ext4", "resize2fs", "blkid"}, pkg: "e2fsprogs"},
	CapXFS:  {bins: []string{"mkfs.xfs", "xfs_growfs", "blkid"}, pkg: "xfsprogs"},
	CapNVMe: {bins: []string{"nvme"}, mods: []string{"nvme_tcp"}, pkg: "nvme-cli"},
	// mount.cifs is the binary that matters: mount(8) hands a -t cifs mount
	// straight to it, and without it the mount fails with "unknown filesystem
	// type" no matter what the kernel supports.
	CapSMB:       {bins: []string{"mount.cifs"}, mods: []string{"cifs"}, pkg: "cifs-utils"},
	CapMultipath: {bins: []string{"multipath", "multipathd"}, mods: []string{"dm_multipath"}, pkg: "multipath-tools"},
}

// capabilityOrder fixes the iteration order so labels and log lines are stable.
var capabilityOrder = []Capability{CapNFS, CapISCSI, CapNVMe, CapSMB, CapExt4, CapXFS, CapMultipath}

// CapabilityOrder is every capability the node plugin probes and publishes a
// topology label for, in a stable order. Anything that has to agree with the
// node's published topology reads it from here rather than restating the list.
func CapabilityOrder() []Capability { return append([]Capability(nil), capabilityOrder...) }

// Preflight is the result of probing one node.
type Preflight struct {
	// Found reports, per capability, whether the node can deliver it. Every known
	// capability has an entry, so a missing key means an unknown capability.
	Found map[Capability]bool
	// Missing lists, per unavailable capability, what an operator must install or
	// provide. Entries are package names, or a module name when the kernel object is
	// absent from disk entirely and no package can be named.
	Missing map[Capability][]string
}

// ModprobeFunc loads a kernel module by name on the host.
type ModprobeFunc func(ctx context.Context, mod string) error

// Detect probes the host filesystem rooted at root — "/host" in the DaemonSet, where
// the host's / is mounted — and reports what this node can do.
//
// A module that is on disk but not loaded is a recoverable state, not an unavailable
// one: Detect calls modprobe for it and, on success, counts the capability as
// available. A module that is neither loaded nor on disk makes the capability
// unavailable. modprobe may be nil, in which case any load attempt fails and the
// capability is reported unavailable rather than silently assumed.
func Detect(ctx context.Context, root string, modprobe ModprobeFunc) (*Preflight, error) {
	if modprobe == nil {
		modprobe = func(context.Context, string) error {
			return errors.New("no modprobe available in this process")
		}
	}

	loaded, err := loadedModules(root)
	if err != nil {
		return nil, err
	}
	release := kernelRelease(root)

	p := &Preflight{
		Found:   make(map[Capability]bool, len(requirements)),
		Missing: make(map[Capability][]string),
	}

	for _, c := range capabilityOrder {
		req := requirements[c]
		var missing []string

		for _, b := range req.bins {
			if findBinary(root, b) == "" {
				missing = appendUnique(missing, req.pkg)
			}
		}

		for _, m := range req.mods {
			if loaded[m] {
				continue
			}
			// Not loaded. Recoverable only if the object exists on disk.
			if !moduleOnDisk(root, release, m) {
				missing = appendUnique(missing, fmt.Sprintf("kernel module %s", m))
				continue
			}
			if err := modprobe(ctx, m); err != nil {
				missing = appendUnique(missing, fmt.Sprintf("kernel module %s (present but modprobe failed: %v)", m, err))
			}
		}

		if len(missing) == 0 {
			p.Found[c] = true
			continue
		}
		p.Found[c] = false
		sort.Strings(missing)
		p.Missing[c] = missing
	}

	return p, nil
}

// Require returns nil when the node can deliver c, and otherwise an error naming what
// must be installed, wrapping ErrCapabilityUnavailable. It exists so NodeStageVolume
// fails fast with an actionable message instead of a cryptic mount error.
func (p *Preflight) Require(c Capability) error {
	if p == nil {
		return fmt.Errorf("%w: %s was never probed on this node", ErrCapabilityUnavailable, c)
	}
	if p.Found[c] {
		return nil
	}
	missing := p.Missing[c]
	if len(missing) == 0 {
		return fmt.Errorf("%w: %s is not available on this node", ErrCapabilityUnavailable, c)
	}
	return fmt.Errorf("%w: %s needs %s on this node", ErrCapabilityUnavailable, c, strings.Join(missing, ", "))
}

// findBinary returns the absolute path of name under one of the host's binary
// directories, or "" when it is present in none of them.
func findBinary(root, name string) string {
	for _, d := range binDirs {
		p := filepath.Join(root, d, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p
	}
	return ""
}

// loadedModules reads proc/modules under root. A missing file is not an error: it means
// the host procfs is not mounted into this container, and every module then falls back
// to the on-disk check.
func loadedModules(root string) (map[string]bool, error) {
	f, err := os.Open(filepath.Join(root, "proc", "modules"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("read proc/modules under %s: %w", root, err)
	}
	defer func() { _ = f.Close() }()

	mods := map[string]bool{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		name, _, _ := strings.Cut(strings.TrimSpace(s.Text()), " ")
		if name == "" {
			continue
		}
		mods[normalizeModule(name)] = true
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("scan proc/modules under %s: %w", root, err)
	}
	return mods, nil
}

// kernelRelease reads the host's kernel release, preferring the host's own
// proc/sys/kernel/osrelease over this container's uname, which reports the same kernel
// but is unavailable when procfs is not mounted.
func kernelRelease(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "proc", "sys", "kernel", "osrelease"))
	if err == nil {
		if rel := strings.TrimSpace(string(b)); rel != "" {
			return rel
		}
	}
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// moduleOnDisk reports whether the kernel object for mod exists under
// lib/modules/<release>. Module file names use hyphens where /proc/modules uses
// underscores (dm-multipath.ko is dm_multipath), so both spellings are accepted, as are
// the compressed .ko.xz, .ko.gz and .ko.zst forms.
func moduleOnDisk(root, release, mod string) bool {
	if release == "" {
		return false
	}
	base := filepath.Join(root, "lib", "modules", release)
	want := normalizeModule(mod)

	found := false
	// The walk ignores errors on individual entries: an unreadable subtree must not
	// mask a module present elsewhere.
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		idx := strings.Index(name, ".ko")
		if idx <= 0 {
			return nil
		}
		switch name[idx:] {
		case ".ko", ".ko.xz", ".ko.gz", ".ko.zst":
		default:
			return nil
		}
		if normalizeModule(name[:idx]) == want {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// normalizeModule folds the hyphen/underscore difference between module file names and
// the /proc/modules listing.
func normalizeModule(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

func appendUnique(list []string, s string) []string {
	for _, existing := range list {
		if existing == s {
			return list
		}
	}
	return append(list, s)
}
