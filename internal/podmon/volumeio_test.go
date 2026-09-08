package podmon

// The fixtures under testdata/host are captured procfs content, not invented
// shapes: /proc/diskstats with its post-4.18 discard and flush columns, an NFSv4
// entry from /proc/self/mountstats complete with its per-operation block, and a
// cifs entry, which publishes bytes and nothing else. Parsing that is exercised
// against the real column order is the whole point — a latency figure taken from
// the wrong column is not a failure any deployment would notice.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

const (
	iscsiMount = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.truenas.watteel.com/iscsi/globalmount"
	nfsMount   = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.truenas.watteel.com/nfs/globalmount"
	smbMount   = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.truenas.watteel.com/smb/globalmount"
)

func fixtureStats(t *testing.T) *IOStats {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "host"))
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}
	return NewIOStats(root)
}

func TestSampleFromCapturedProcContent(t *testing.T) {
	tests := []struct {
		name       string
		mountPath  string
		devicePath string
		wantOK     bool
		want       obs.VolumeIOSample
	}{
		{
			// An iSCSI volume with a filesystem: found through the mount table,
			// counted by /proc/diskstats. Sectors are 512 bytes whatever the
			// device's logical block size, and the millisecond columns are
			// cumulative service time, never an average.
			name:      "iscsi filesystem volume from diskstats",
			mountPath: iscsiMount,
			wantOK:    true,
			want: obs.VolumeIOSample{
				ReadOps: 45231, WriteOps: 78412,
				ReadBytes: 3617848 * 512, WriteBytes: 6273016 * 512,
				ReadSeconds: 12.903, WriteSeconds: 98.211,
				BusySeconds: 41.2,
				HasOps:      true, HasLatency: true, HasBusy: true,
			},
		},
		{
			// A raw block volume has no filesystem mount at all, so it is named
			// by device instead. This is the NVMe shape.
			name:       "raw block volume named by device",
			devicePath: "/dev/nvme0n1",
			wantOK:     true,
			want: obs.VolumeIOSample{
				ReadOps: 88123, WriteOps: 51234,
				ReadBytes: 7048984 * 512, WriteBytes: 4098752 * 512,
				ReadSeconds: 9.12, WriteSeconds: 33.21,
				BusySeconds: 30.11,
				HasOps:      true, HasLatency: true, HasBusy: true,
			},
		},
		{
			// NFS: bytes are the SERVER columns (what crossed the wire) and
			// latency is the per-operation RTT, which is a measured round trip
			// rather than a proxy for one.
			name:      "nfs mount from mountstats",
			mountPath: nfsMount,
			wantOK:    true,
			want: obs.VolumeIOSample{
				ReadOps: 3125, WriteOps: 1536,
				ReadBytes: 98566144, WriteBytes: 50331648,
				ReadSeconds: 9.351, WriteSeconds: 24.010,
				HasOps: true, HasLatency: true,
			},
		},
		{
			// cifs counts bytes and nothing else, so no operation counters and
			// no latency are exported rather than fabricated ones.
			name:      "smb mount reports bytes only",
			mountPath: smbMount,
			wantOK:    true,
			want: obs.VolumeIOSample{
				ReadBytes: 20971520, WriteBytes: 10485760,
			},
		},
		{
			name:      "unknown path yields no sample",
			mountPath: "/var/lib/kubelet/plugins/kubernetes.io/csi/nope/globalmount",
		},
		{
			name:   "no path and no device yields no sample",
			wantOK: false,
		},
	}

	s := fixtureStats(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.Sample(tc.mountPath, tc.devicePath)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got != tc.want {
				t.Fatalf("sample =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// TestDeviceNameFollowsSymlinks pins the resolution a multipath or by-id mount
// needs. /proc/mounts names /dev/mapper/mpatha; /proc/diskstats knows only
// dm-0, so a driver that took the basename would silently report nothing for
// every multipath volume.
func TestDeviceNameFollowsSymlinks(t *testing.T) {
	links := map[string]string{
		"/dev/mapper/mpatha":                 "../dm-0",
		"/dev/disk/by-id/scsi-36589cfc00000": "../../sdc",
		"/dev/disk/by-id/two-hops":           "/dev/mapper/mpatha",
	}
	s := &IOStats{
		root:     "/host",
		readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		readLink: func(p string) (string, error) {
			target, ok := links[filepath.Clean("/"+p[len("/host"):])]
			if !ok {
				return "", errors.New("not a symlink")
			}
			return target, nil
		},
	}
	for _, tc := range []struct{ path, want string }{
		{"/dev/sdc", "sdc"},
		{"/dev/mapper/mpatha", "dm-0"},
		{"/dev/disk/by-id/scsi-36589cfc00000", "sdc"},
		{"/dev/disk/by-id/two-hops", "dm-0"},
	} {
		if got := s.deviceName(tc.path); got != tc.want {
			t.Fatalf("deviceName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestSampleIsRawAndSurvivesACounterReset pins that nothing here smooths,
// clamps or remembers. A remount restarts the kernel's counters from zero, and
// the exporter must publish the smaller value as it stands: Prometheus detects
// the reset and rate() accounts for it, whereas an exporter that clamped to the
// previous maximum would invent traffic that never happened.
func TestSampleIsRawAndSurvivesACounterReset(t *testing.T) {
	before := "   8      32 sdc 100 0 200 10 50 0 300 20 0 30 40\n"
	after := "   8      32 sdc 1 0 2 1 1 0 3 1 0 1 1\n" // remounted; counters reset
	content := before
	s := &IOStats{
		root:     "/host",
		readFile: func(string) ([]byte, error) { return []byte(content), nil },
		readLink: func(string) (string, error) { return "", errors.New("not a symlink") },
	}

	first, ok := s.Sample("", "/dev/sdc")
	if !ok || first.ReadOps != 100 {
		t.Fatalf("first sample = %+v, ok=%v", first, ok)
	}
	content = after
	second, ok := s.Sample("", "/dev/sdc")
	if !ok {
		t.Fatal("a reset counter must still be readable")
	}
	if second.ReadOps != 1 || second.ReadBytes != 2*sectorSize {
		t.Fatalf("a reset must be reported as it stands, got %+v", second)
	}
}

// TestActivityCountsCachedTraffic keeps the "has this volume done any I/O"
// probe as sensitive as it was before the exported metrics shared its parser.
// A read served from the client's page cache never reaches the NAS, so it does
// not move the server byte counters the metrics export — but it is still the
// pod doing I/O, and calling that idleness would let a busy pod be fenced.
func TestActivityCountsCachedTraffic(t *testing.T) {
	body := []string{"\tbytes:\t4096 0 0 0 0 0 1 0"}
	v, ok := nfsStats(body)
	if !ok {
		t.Fatal("a bytes: line must produce a reading")
	}
	if v.readBytes != 0 {
		t.Fatalf("server read bytes = %d, want 0: nothing crossed the wire", v.readBytes)
	}
	if v.activity() == 0 {
		t.Fatal("a cached read must still count as activity")
	}
}
