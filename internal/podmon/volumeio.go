package podmon

// Per-volume I/O counters, read from the node's own kernel.
//
// The appliance cannot answer this question — verified on hardware 2026-09-08,
// `reporting.netdata_graphs` has no per-dataset, per-zvol or per-pool series at
// all (see .procoder/notes/truenas-api-findings.md) — but the node it is used
// on counts every operation the volume performs:
//
//   - iSCSI and NVMe volumes are block devices, so /proc/diskstats has
//     completed reads and writes, sectors moved, milliseconds spent servicing
//     each direction, and io_ticks;
//   - NFS mounts appear in /proc/self/mountstats with wire byte counters and
//     per-operation RPC statistics, which carry a real round-trip time rather
//     than a proxy for one;
//   - SMB mounts appear there too, but the cifs client publishes bytes only —
//     no operation counts and no latency.
//
// Everything read here is a plain file read of a procfs file. Nothing in this
// file stats, opens or otherwise touches the mount itself, which is what makes
// it safe to run from a Prometheus scrape: the failure this driver exists to
// survive is exactly the one that makes statfs(2) on an NFS mount block for
// ever, and a scrape that could hang would take the node's own metrics down
// with the storage.

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// sectorSize is the unit /proc/diskstats counts in. It is 512 bytes whatever
// the device's real logical block size — the kernel normalises it — so it is a
// constant rather than something to look up in sysfs.
const sectorSize = 512

// IOStats reads per-volume kernel I/O counters under a filesystem root, which
// is the host root the node DaemonSet mounts (node.DefaultHostRoot) in
// production and a fixture directory in tests.
type IOStats struct {
	root string

	// readFile and readLink are fields so tests can supply captured procfs
	// content, and so this file has no build tag: on darwin the files simply do
	// not exist and every read fails cleanly.
	readFile func(string) ([]byte, error)
	readLink func(string) (string, error)
}

// NewIOStats builds a reader rooted at the host filesystem.
func NewIOStats(root string) *IOStats {
	return &IOStats{root: root, readFile: os.ReadFile, readLink: os.Readlink}
}

// Sample returns one volume's cumulative counters.
//
// mountPath is the volume's staging path — the mount the kubelet later bind
// mounts into the pod, so it and the pod's own path share one set of kernel
// counters. devicePath is set only for a raw block volume, which has no
// filesystem mount to look up and must therefore be named directly.
//
// The second result is false when nothing could be read: a volume staged but
// not yet mounted, a device that has gone away, or a kernel without the
// statistics compiled in. The caller counts that rather than exporting zeros,
// because a flat zero counter reads as "idle" when the truth is "unknown".
func (s *IOStats) Sample(mountPath, devicePath string) (obs.VolumeIOSample, bool) {
	v, ok := s.read(mountPath, devicePath)
	if !ok {
		return obs.VolumeIOSample{}, false
	}
	return v.sample(), true
}

// volumeIO is every counter this package can extract for one volume. It is
// richer than obs.VolumeIOSample because the "has there been any I/O at all"
// probe in iocounters.go wants maximum sensitivity — client-side cached traffic
// included — while the exported metrics want figures with a defensible meaning.
type volumeIO struct {
	readOps, writeOps       uint64
	readBytes, writeBytes   uint64
	readTicks, writeTicks   uint64 // milliseconds spent, cumulative
	busyTicks               uint64 // milliseconds with I/O in flight
	cachedRead, cachedWrite uint64 // NFS traffic that never reached the server

	hasOps, hasLatency, hasBusy bool
}

// sample projects the counters onto the exported metric set.
func (v volumeIO) sample() obs.VolumeIOSample {
	return obs.VolumeIOSample{
		ReadOps:      float64(v.readOps),
		WriteOps:     float64(v.writeOps),
		ReadBytes:    float64(v.readBytes),
		WriteBytes:   float64(v.writeBytes),
		ReadSeconds:  float64(v.readTicks) / 1000,
		WriteSeconds: float64(v.writeTicks) / 1000,
		BusySeconds:  float64(v.busyTicks) / 1000,
		HasOps:       v.hasOps,
		HasLatency:   v.hasLatency,
		HasBusy:      v.hasBusy,
	}
}

// activity is the single number the "did anything move" probe compares between
// polls. It deliberately sums every counter, cached traffic included: a read
// served from the client's page cache is still the pod doing I/O, and treating
// it as idleness would let a busy pod be declared dead.
func (v volumeIO) activity() uint64 {
	return v.readOps + v.writeOps + v.readBytes + v.writeBytes + v.cachedRead + v.cachedWrite
}

// read resolves the volume to a source and parses it.
func (s *IOStats) read(mountPath, devicePath string) (volumeIO, bool) {
	if devicePath != "" {
		if v, ok := s.diskstats(s.deviceName(devicePath)); ok {
			return v, true
		}
	}
	if mountPath == "" {
		return volumeIO{}, false
	}
	if src, ok := s.mountSource(mountPath); ok && strings.HasPrefix(src, "/dev/") {
		return s.diskstats(s.deviceName(src))
	}
	return s.mountstats(mountPath)
}

// deviceName reduces a device path to the name /proc/diskstats uses.
//
// The mount table names the device the mount was made with, which for a
// multipath volume is /dev/mapper/mpathX and for a by-id attach is a
// /dev/disk/by-id/ symlink — neither of which appears in diskstats. Following
// the symlink yields the dm-N or sdX name that does. Two hops are enough for
// every shape the node itself creates, and a name that is not a symlink is used
// as it stands.
func (s *IOStats) deviceName(path string) string {
	for i := 0; i < 2; i++ {
		target, err := s.readLink(filepath.Join(s.root, path))
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return filepath.Base(path)
}

// mountSource returns the device or server:/export a path is mounted from.
func (s *IOStats) mountSource(path string) (string, bool) {
	b, err := s.readFile(filepath.Join(s.root, "proc", "mounts"))
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || filepath.Clean(unescapeOctal(f[1])) != clean {
			continue
		}
		return unescapeOctal(f[0]), true
	}
	return "", false
}

// diskstats parses one device's line of /proc/diskstats.
//
// Field order is the kernel's (Documentation/admin-guide/iostats.rst), 1-based
// after the major, minor and name: reads completed, reads merged, sectors read,
// milliseconds reading, writes completed, writes merged, sectors written,
// milliseconds writing, I/Os in progress, milliseconds doing I/O, weighted
// milliseconds. Later kernels append discard and flush fields, which are
// ignored rather than parsed, so a newer line still reads correctly.
func (s *IOStats) diskstats(device string) (volumeIO, bool) {
	if device == "" {
		return volumeIO{}, false
	}
	b, err := s.readFile(filepath.Join(s.root, "proc", "diskstats"))
	if err != nil {
		return volumeIO{}, false
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 14 || f[2] != device {
			continue
		}
		n := make([]uint64, 0, 11)
		for _, v := range f[3:14] {
			parsed, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return volumeIO{}, false
			}
			n = append(n, parsed)
		}
		return volumeIO{
			readOps:    n[0],
			readBytes:  n[2] * sectorSize,
			readTicks:  n[3],
			writeOps:   n[4],
			writeBytes: n[6] * sectorSize,
			writeTicks: n[7],
			busyTicks:  n[9],
			hasOps:     true,
			hasLatency: true,
			hasBusy:    true,
		}, true
	}
	return volumeIO{}, false
}

// mountstats parses the /proc/self/mountstats section for one mount point.
func (s *IOStats) mountstats(path string) (volumeIO, bool) {
	b, err := s.readFile(filepath.Join(s.root, "proc", "self", "mountstats"))
	if err != nil {
		return volumeIO{}, false
	}
	fstype, body, ok := mountSection(string(b), path)
	if !ok {
		return volumeIO{}, false
	}
	if strings.HasPrefix(fstype, "nfs") {
		return nfsStats(body)
	}
	if strings.HasPrefix(fstype, "cifs") || strings.HasPrefix(fstype, "smb") {
		return cifsStats(body)
	}
	return volumeIO{}, false
}

// mountSection returns the filesystem type and the indented body of the
// mountstats entry for one mount point.
func mountSection(content, path string) (fstype string, body []string, ok bool) {
	clean := filepath.Clean(path)
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "device ") {
			if ok {
				body = append(body, line)
			}
			continue
		}
		if ok {
			return fstype, body, true // the next device ends our section
		}
		// "device <src> mounted on <target> with fstype <fs> [statvers=…]"
		f := strings.Fields(line)
		var target, fs string
		for i, w := range f {
			switch {
			case w == "on" && i+1 < len(f):
				target = unescapeOctal(f[i+1])
			case w == "fstype" && i+1 < len(f):
				fs = f[i+1]
			}
		}
		if filepath.Clean(target) == clean {
			fstype, ok = fs, true
		}
	}
	return fstype, body, ok
}

// nfsStats reads an NFS mount's byte counters and per-operation RPC statistics.
//
//	bytes: <normal read> <normal write> <direct read> <direct write>
//	       <server read> <server write> <read pages> <write pages>
//
// The exported byte counters are the SERVER figures — what actually crossed the
// wire — because those are the analogue of the array-side numbers Dell reports.
// The normal (client) figures are kept for the activity probe only: a read
// served from the page cache is real work by the pod but no work by the NAS.
//
// The per-operation block gives, per RPC:
//
//	<OP>: ops trans timeouts bytes_sent bytes_recv queue_ms rtt_ms execute_ms [errors]
//
// rtt_ms is the server's response time and is what this exports as latency.
// execute_ms would add the client's own queueing, which measures the node's
// backlog rather than the storage.
func nfsStats(body []string) (volumeIO, bool) {
	var v volumeIO
	var seen bool
	for _, line := range body {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "bytes:" && len(f) >= 7:
			nums := parseUints(f[1:])
			if len(nums) < 6 {
				continue
			}
			v.cachedRead, v.cachedWrite = nums[0], nums[1]
			v.readBytes, v.writeBytes = nums[4], nums[5]
			seen = true
		case f[0] == "READ:" && len(f) >= 8:
			nums := parseUints(f[1:])
			if len(nums) < 7 {
				continue
			}
			v.readOps, v.readTicks = nums[0], nums[6]
			v.hasOps, v.hasLatency, seen = true, true, true
		case f[0] == "WRITE:" && len(f) >= 8:
			nums := parseUints(f[1:])
			if len(nums) < 7 {
				continue
			}
			v.writeOps, v.writeTicks = nums[0], nums[6]
			v.hasOps, v.hasLatency, seen = true, true, true
		}
	}
	return v, seen
}

// cifsStats reads an SMB mount's counters.
//
// The cifs client publishes far less than the NFS one. Current kernels print
//
//	Bytes read: <n>  Bytes written: <n>
//
// and older ones a per-operation form, "Reads: <ops> Bytes: <n>". Both are
// accepted; neither carries a latency figure, so an SMB volume is exported with
// bytes only rather than with a fabricated one.
func cifsStats(body []string) (volumeIO, bool) {
	var v volumeIO
	var seen bool
	for _, line := range body {
		f := strings.Fields(line)
		for i, w := range f {
			switch {
			case w == "Bytes" && i+2 < len(f) && f[i+1] == "read:":
				if n, err := strconv.ParseUint(f[i+2], 10, 64); err == nil {
					v.readBytes, seen = n, true
				}
			case w == "Bytes" && i+2 < len(f) && f[i+1] == "written:":
				if n, err := strconv.ParseUint(f[i+2], 10, 64); err == nil {
					v.writeBytes, seen = n, true
				}
			case w == "Reads:" && i+3 < len(f) && f[i+2] == "Bytes:":
				v.readOps, _ = strconv.ParseUint(f[i+1], 10, 64)
				if n, err := strconv.ParseUint(f[i+3], 10, 64); err == nil {
					v.readBytes, v.hasOps, seen = n, true, true
				}
			case w == "Writes:" && i+3 < len(f) && f[i+2] == "Bytes:":
				v.writeOps, _ = strconv.ParseUint(f[i+1], 10, 64)
				if n, err := strconv.ParseUint(f[i+3], 10, 64); err == nil {
					v.writeBytes, v.hasOps, seen = n, true, true
				}
			}
		}
	}
	return v, seen
}

// parseUints parses as many leading unsigned integers as it can, stopping at
// the first field that is not one. Trailing free text — the "errors" column
// some kernels add, a units suffix — therefore costs nothing.
func parseUints(fields []string) []uint64 {
	out := make([]uint64, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// unescapeOctal decodes the \040-style escapes /proc uses in paths.
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
