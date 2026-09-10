package node

// The stage record: what this node remembers about a volume it staged.
//
// NodeUnstageVolume carries no publish context, so without a record on disk the
// node cannot know which target to log out of — and guessing from live session
// state is not an option, because Longhorn shares this node's iSCSI stack and a
// wrong guess would tear down its sessions.
//
// The record also carries the volume id, and that is what makes RECOVERY
// possible. The health monitor's targets live in memory and are registered by
// NodeStageVolume alone, so every restart of the node plugin — an upgrade, an
// OOM kill, a crash — left every volume already on the node unwatched. The
// kubelet does not help: its own state says those volumes are staged, so it
// never calls NodeStageVolume again, and the monitor stays empty until each
// volume is unstaged and staged afresh. In practice that means never.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// stageRecord is the on-disk form. The volume id is stored beside the publish
// context rather than inside it, so it can never be mistaken for a publish key.
type stageRecord struct {
	VolumeID string `json:"volumeID,omitempty"`
	// Kind says which call wrote this record: recordStage beside a staging
	// path, recordPublish beside a pod's target path.
	//
	// It is recorded rather than inferred from the path because the paths are
	// the CO's to choose. Reading "globalmount" out of a path works only
	// because Kubernetes happens to spell it that way, and a driver that
	// depends on that is a driver that breaks on a CO which does not — and
	// silently, by monitoring a pod's mount that vanishes with the pod instead
	// of the staging mount that lives as long as the volume.
	Kind           string            `json:"kind,omitempty"`
	PublishContext map[string]string `json:"publishContext,omitempty"`
}

const (
	recordStage   = "stage"
	recordPublish = "publish"
)

// stageRecordSuffix names the file. It is also how a mount is recognised as one
// of this driver's during recovery: the file exists only where this driver put
// it, which is a better test than any pattern over kubelet paths, whose layout
// is the kubelet's business and not this driver's.
const stageRecordSuffix = ".truenas-csi.json"

// StageRecordPath is where the record for one staging or target path lives.
func StageRecordPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+stageRecordSuffix)
}

// WriteStageRecord remembers how to detach a volume, and which volume it is.
// It is written beside the STAGING path, which lives as long as the volume is
// on this node.
func WriteStageRecord(path, volumeID string, pc map[string]string) error {
	return writeRecord(path, stageRecord{
		VolumeID: volumeID, Kind: recordStage, PublishContext: pc})
}

// WritePublishRecord remembers the same context beside a POD's target path,
// which NodeExpandVolume is called with and which may carry no staging path.
// It disappears with the pod.
func WritePublishRecord(path, volumeID string, pc map[string]string) error {
	return writeRecord(path, stageRecord{
		VolumeID: volumeID, Kind: recordPublish, PublishContext: pc})
}

func writeRecord(path string, r stageRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(StageRecordPath(path), b, 0o600)
}

// ReadStageRecord returns the publish context recorded for a path, or nil.
func ReadStageRecord(path string) map[string]string {
	r, ok := readStageRecord(path)
	if !ok {
		return nil
	}
	return r.PublishContext
}

// readStageRecord decodes a record in either the current shape or the flat map
// this driver used to write.
//
// The old shape is still accepted because a running node has records on disk
// from before the upgrade, and refusing them would break the very thing they
// exist for: unstaging a volume the previous version staged. Such a record
// carries no volume id, so it can be unstaged but not recovered — which is
// exactly the behaviour before this file, for one cycle of each volume.
func readStageRecord(path string) (stageRecord, bool) {
	b, err := os.ReadFile(StageRecordPath(path))
	if err != nil {
		return stageRecord{}, false
	}
	var r stageRecord
	if err := json.Unmarshal(b, &r); err == nil && r.PublishContext != nil {
		return r, true
	}
	var flat map[string]string
	if err := json.Unmarshal(b, &flat); err != nil || flat == nil {
		return stageRecord{}, false
	}
	return stageRecord{PublishContext: flat}, true
}

// RecoverStagedVolumes re-registers, with the health monitor and the I/O metric
// sink, every volume this node had staged before the process started.
//
// Volumes are found through the host's own mount table rather than by walking
// the kubelet's directories: a mount is what proves a volume is still in use
// here, and the record beside a mount point is what identifies it as ours. Both
// the staging mount of a filesystem volume and the published bind mount of a
// raw block volume carry a record, so both are found.
func (n *Node) RecoverStagedVolumes(ctx context.Context) int {
	entries, err := n.mounts()
	if err != nil {
		obs.Logger(ctx).Warn("could not read the host mount table; volumes staged before "+
			"this process started will not be monitored until they are staged again",
			"error", obs.Redact(err.Error()))
		return 0
	}

	// A volume appears twice when it is both staged and published. The staging
	// mount is the one to keep: it is the path the health monitor stats, and
	// the published path of a raw block volume is a device node, not a
	// filesystem.
	type found struct {
		req  StageRequest
		kind string
	}
	best := map[string]found{}
	for _, e := range entries {
		r, ok := readStageRecord(e.target)
		if !ok || r.VolumeID == "" {
			continue
		}
		// A staging record beats a publish record: the staging mount lives as
		// long as the volume is on this node, while a pod's mount goes away
		// with the pod and would leave the monitor stat'ing a path that is
		// gone — reporting a healthy volume as unreachable.
		if prev, seen := best[r.VolumeID]; seen && prev.kind == recordStage {
			continue
		}
		best[r.VolumeID] = found{
			req: StageRequest{
				VolumeID:       r.VolumeID,
				StagingPath:    e.target,
				PublishContext: r.PublishContext,
				// A raw block volume's mount point IS the device node. That is
				// the fact that identifies it, and healthPathOf then answers ""
				// because there is no filesystem to stat.
				VolumeCapability: VolumeCapability{Block: n.blockDeviceAt(e.target) != ""},
			},
			kind: r.Kind,
		}
	}

	for _, f := range best {
		req := f.req
		proto := protocolOf(req.PublishContext)
		n.health.Track(HealthTarget{
			VolumeID: req.VolumeID,
			Protocol: proto,
			Path:     healthPathOf(req),
			Backend:  backendOf(req.VolumeID),
			DataAddr: dataAddrOf(req.PublishContext),
			NAA:      req.PublishContext[KeyNAA],
			Portal:   req.PublishContext[KeyPortal],
			IQN:      req.PublishContext[KeyIQN],
			LUN:      req.PublishContext[KeyLUN],
		})
		n.trackIO(ctx, req, proto)
	}
	if len(best) > 0 {
		obs.Logger(ctx).Info("resumed monitoring volumes staged before this process started",
			"volumes", len(best))
	}
	return len(best)
}
