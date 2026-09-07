package node

// Multipath.
//
// Multipath is the one capability where degrading is the correct behaviour. Every
// other missing package fails the stage with a message naming it, because mounting
// xfs without xfsprogs cannot work. A volume without multipathd works perfectly
// well over its single path — it simply has no redundancy — so a missing
// multipath-tools must never fail an attach. It warns, once, and continues.

import (
	"context"
	"strings"

	"github.com/pwatteel/truenas-csi/internal/obs"
)

// mapperDir is the host-absolute directory device-mapper publishes maps under.
const mapperDir = "/dev/mapper"

// multipathDevice asks multipathd which map, if any, fronts the LUN with this NAA,
// and returns the mapper path for it. The second result is false — with no error —
// when the LUN is simply not multipathed, which is the normal case on a single-path
// node and must not be treated as a failure.
//
// The command is scoped to one wwid. `multipath -ll` with no argument walks every
// map on the host, and on a node shared with another storage driver that is both
// slow and needless.
func multipathDevice(ctx context.Context, e Executor, naa string) (string, bool, error) {
	wwid := multipathWWID(naa)
	if wwid == "" {
		return "", false, nil
	}
	out, err := e.Run(ctx, "multipath", "-l", wwid)
	if err != nil {
		// multipath exits non-zero when it knows nothing about the wwid. That is
		// an answer, not a fault: fall back to the single path.
		return "", false, nil
	}
	name := parseMultipathName(string(out), wwid)
	if name == "" {
		return "", false, nil
	}
	return mapperDir + "/" + name, true, nil
}

// multipathWWID renders the NAA as the wwid multipathd knows it by: the SCSI page
// 0x83 identifier, which for these extents is the NAA prefixed with its designator
// type 3 — the same "3<naa>" that appears in the by-id link.
func multipathWWID(naa string) string {
	id := normalizeNAA(naa)
	if id == "" {
		return ""
	}
	return "3" + id
}

// parseMultipathName reads the map name out of multipath -l's header line. Two
// shapes exist, depending on user_friendly_names:
//
//	mpatha (36589cfc000000a960e31390c2657efa7) dm-3 TrueNAS ,iSCSI Disk
//	36589cfc000000a960e31390c2657efa7 dm-3 TrueNAS ,iSCSI Disk
//
// Anything that does not look like one of those — an empty output, a "no such
// map" message, an indented path line — yields "", and the caller falls back.
func parseMultipathName(out, wwid string) string {
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '|' || line[0] == '`' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		switch {
		case strings.HasPrefix(fields[1], "("):
			// Aliased form: the wwid is parenthesised after the alias.
			if strings.Trim(fields[1], "()") != wwid {
				continue
			}
			return name
		case name == wwid:
			return name
		}
	}
	return ""
}

// warnNoMultipath emits the degradation warning at most once for the life of this
// process. Once per node start, not once per volume: a node without
// multipath-tools would otherwise repeat the same line on every single attach and
// bury everything else in the log.
func (n *Node) warnNoMultipath(ctx context.Context) {
	n.multipathWarn.Do(func() {
		obs.Logger(ctx).Warn(
			"multipath is unavailable on this node; iSCSI volumes will attach over a single path with no path redundancy. Install multipath-tools and load dm_multipath to enable it",
			"capability", string(CapMultipath),
			"package", "multipath-tools",
		)
	})
}
