package csi

import (
	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// accessClass is what decides how many nodes a volume may be served to at once.
//
// The distinction is NOT a list of protocol names, because the property that
// matters is not the protocol: it is whether the appliance hands out a
// filesystem it arbitrates itself, or a raw block device whose filesystem lives
// in each client's kernel.
//
//   - A shared filesystem (NFS, SMB) is served by a server that owns the
//     metadata and the locks. Serving it to twenty clients at once is the
//     normal, designed use.
//   - A raw block device (a zvol behind an iSCSI LUN or an NVMe-oF namespace)
//     is a byte range. Two nodes mounting ext4 on it read-write both believe
//     they own the journal and the allocation bitmaps, and the filesystem is
//     destroyed — quietly, and usually not at the moment of the second mount.
//
// So a protocol is classified by which of those two things it is, and anything
// this driver has not deliberately classified is treated as raw block: the
// failure mode of guessing "shared" is silent data loss, and the failure mode
// of guessing "block" is a PVC that stays Pending until somebody classifies it.
type accessClass int

const (
	// classRawBlock is a block device the client's own kernel puts a
	// filesystem on. The zero value, so an unclassified protocol is safe.
	classRawBlock accessClass = iota
	// classSharedFilesystem is a filesystem the appliance serves and arbitrates.
	classSharedFilesystem
)

// protocolAccessClass classifies every protocol this driver ships.
//
// A protocol added without an entry here is refused every MULTI_NODE mode
// rather than silently granted one; TestEveryShippedProtocolIsClassified fails
// the build until the new protocol is classified on purpose.
var protocolAccessClass = map[string]accessClass{
	"nfs":   classSharedFilesystem,
	"smb":   classSharedFilesystem,
	"iscsi": classRawBlock,
	"nvme":  classRawBlock,
}

// accessClassOf classifies a protocol, defaulting to raw block.
func accessClassOf(protocol string) accessClass {
	return protocolAccessClass[protocol]
}

// supportsAccessMode reports whether a protocol can honestly serve an access
// mode.
//
// A shared filesystem admits every mode the spec defines. Everything else is
// confined to the SINGLE_NODE family — including SINGLE_NODE_MULTI_WRITER,
// which is several pods on ONE node and is safe on a block device, unlike the
// MULTI_NODE modes, which are several nodes.
func supportsAccessMode(protocol string, m csipb.VolumeCapability_AccessMode_Mode) bool {
	if accessClassOf(protocol) == classSharedFilesystem {
		return m != csipb.VolumeCapability_AccessMode_UNKNOWN
	}
	return singleNode(m)
}

// singleNode reports whether an access mode admits at most one node.
func singleNode(m csipb.VolumeCapability_AccessMode_Mode) bool {
	switch m {
	case csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return true
	}
	return false
}

// requireSupportedCapabilities refuses capabilities a protocol cannot serve.
//
// NFS and SMB serve a filesystem the appliance arbitrates and cannot hand a
// node a block device. Accepting volumeMode: Block against them BOUND the claim
// and provisioned a dataset nothing could ever use: the pod then sat Pending
// with MapVolume.MapPodDevice failing "protocol \"nfs\" has no block device".
// The node's refusal was right; the place was wrong. CSI requires CreateVolume
// to answer INVALID_ARGUMENT when the requested capabilities cannot be served,
// so that no volume is created at all.
func requireSupportedCapabilities(protocol string, caps []*csipb.VolumeCapability) error {
	for _, c := range caps {
		if c.GetBlock() != nil {
			if accessClassOf(protocol) == classSharedFilesystem {
				return status.Errorf(codes.InvalidArgument,
					"protocol %q serves a filesystem and cannot provide a raw block device: "+
						"use volumeMode: Filesystem, or a StorageClass whose protocol is iscsi or nvme",
					protocol)
			}
			continue
		}
		// A filesystem this driver cannot make is refused HERE, where the CO
		// puts the message on the PVC. Accepting it and encoding it in the
		// volume's topology instead produces a PV no node can satisfy, and the
		// only thing the user ever sees is a scheduler complaining about a
		// label — never the word "btrfs".
		fs := c.GetMount().GetFsType()
		if fs == "" {
			continue
		}
		if _, ok := node.CapabilityForFS(fs); !ok {
			return status.Errorf(codes.InvalidArgument,
				"filesystem %q is not supported by this driver; use ext4, ext3, ext2 or xfs", fs)
		}
	}
	return nil
}
