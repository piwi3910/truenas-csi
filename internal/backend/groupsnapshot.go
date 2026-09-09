package backend

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GroupSnapshot is a crash-consistent snapshot of several volumes.
//
// "Crash-consistent" is a promise about a single instant: every member must be
// captured in the same ZFS transaction group, exactly as if the machine had
// lost power. A database whose data and WAL volumes were snapshotted a few
// milliseconds apart can restore into a state that never existed, which is why
// this type is only ever produced by one recursive snapshot call.
type GroupSnapshot struct {
	ID           string // <backend>/<parent dataset>@<name>
	Members      []Snapshot
	CreationTime time.Time
	ReadyToUse   bool
}

// CreateGroupSnapshot snapshots every source volume at one instant.
//
// The members must live on one appliance and share a parent dataset, because
// ZFS's only multi-dataset atomic primitive is a recursive snapshot of a common
// ancestor. Both conditions are checked before anything is created: a group
// that cannot be atomic is refused rather than approximated.
func (r *Registry) CreateGroupSnapshot(ctx context.Context, sources []volume.ID, name string) (*GroupSnapshot, error) {
	parent, err := r.groupParent(sources)
	if err != nil {
		return nil, err
	}
	backendName := sources[0].Backend

	c, err := r.Client(ctx, backendName)
	if err != nil {
		return nil, err
	}
	groupZFS := parent + "@" + name

	// Idempotency: the CO retries forever, and a second recursive snapshot
	// under the same name would fail rather than converge.
	existing, err := c.SnapshotQuery(ctx, groupZFS)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		if _, err := c.SnapshotCreateRecursive(ctx, parent, name); err != nil {
			return nil, err
		}
		existing, _ = c.SnapshotQuery(ctx, groupZFS)
	}
	// The creation time is ZFS's, not this process's: a retry must report the
	// same instant as the first call, and only the appliance remembers it
	// across a controller restart. The local clock is the fallback.
	created := time.Now().UTC()
	if existing != nil {
		if t := existing.CreationTime(); !t.IsZero() {
			created = t
		}
	}

	g := &GroupSnapshot{
		ID: snapshotID(backendName, groupZFS), CreationTime: created, ReadyToUse: true,
	}
	for _, s := range sources {
		memberZFS := s.DatasetPath() + "@" + name
		member, _ := c.SnapshotQuery(ctx, memberZFS)
		when := created
		if member != nil {
			if t := member.CreationTime(); !t.IsZero() {
				when = t
			}
		}
		g.Members = append(g.Members, Snapshot{
			ID:             snapshotID(backendName, memberZFS),
			SourceVolumeID: s.String(),
			SizeBytes:      provisionedBytes(ctx, c, member, s.DatasetPath()),
			CreationTime:   when,
			ReadyToUse:     true,
		})
	}
	return g, nil
}

// CheckGroupSources reports whether these volumes can be snapshotted in one
// crash-consistent operation, without creating anything.
//
// The caller uses it to reject an impossible group before it does any other
// work, so a refusal never leaves a partial group on the appliance.
func (r *Registry) CheckGroupSources(sources []volume.ID) error {
	_, err := r.groupParent(sources)
	return err
}

// groupParent returns the dataset the group's recursive snapshot must be taken
// on, or the reason no such dataset exists.
func (r *Registry) groupParent(sources []volume.ID) (string, error) {
	if len(sources) == 0 {
		return "", status.Error(codes.InvalidArgument, "a volume group snapshot needs at least one source volume")
	}
	backendName := sources[0].Backend
	parent := path.Dir(sources[0].DatasetPath())
	for _, s := range sources[1:] {
		if s.Backend != backendName {
			return "", status.Errorf(codes.InvalidArgument,
				"volumes %s and %s live on different appliances (%q and %q); "+
					"a group snapshot cannot be crash-consistent across appliances",
				sources[0], s, backendName, s.Backend)
		}
		if p := path.Dir(s.DatasetPath()); p != parent {
			return "", status.Errorf(codes.FailedPrecondition,
				"volumes %s and %s do not share a parent dataset (%q and %q); "+
					"ZFS can only snapshot several datasets atomically beneath a common parent, "+
					"so this group cannot be made crash-consistent",
				sources[0], s, parent, p)
		}
	}
	// The recursive snapshot lands on the parent dataset itself, so that parent
	// must be the one the operator configured. Without this a handcrafted volume
	// handle could aim a recursive snapshot at an arbitrary part of the pool.
	cfgB, err := r.Backend(backendName)
	if err != nil {
		return "", err
	}
	if want := cfgB.Pool + "/" + cfgB.ParentDataset; parent != want {
		return "", status.Errorf(codes.InvalidArgument,
			"group members live under %q, outside backend %q's parent dataset %q",
			parent, backendName, want)
	}
	return parent, nil
}

// GetGroupSnapshot reports a group snapshot and its members, or NotFound.
func (r *Registry) GetGroupSnapshot(ctx context.Context, id string, memberIDs []string) (*GroupSnapshot, error) {
	backendName, groupZFS, err := parseSnapshotID(id)
	if err != nil {
		return nil, err
	}
	if err := validateGroupMembers(id, backendName, groupZFS, memberIDs); err != nil {
		return nil, err
	}
	c, err := r.Client(ctx, backendName)
	if err != nil {
		return nil, err
	}
	found, err := r.groupSnapshots(ctx, c, groupZFS)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, status.Errorf(codes.NotFound, "group snapshot %s does not exist", id)
	}
	parent, _ := splitSnapshotID(groupZFS)
	g := &GroupSnapshot{ID: id, ReadyToUse: true}
	for i, s := range found {
		if s.Dataset == parent {
			// The parent dataset is the group's anchor, not a volume — but it
			// is the snapshot the group id names, so it carries the group's
			// creation time.
			g.CreationTime = s.CreationTime()
			continue
		}
		g.Members = append(g.Members, Snapshot{
			ID: snapshotID(backendName, s.ID), SourceVolumeID: s.Dataset,
			SizeBytes:    provisionedBytes(ctx, c, &found[i], s.Dataset),
			CreationTime: s.CreationTime(), ReadyToUse: true,
		})
	}
	// Members the caller named take precedence over what the parent happens to
	// hold: a dataset that merely lives alongside the group is not a member.
	if len(memberIDs) > 0 {
		want := map[string]bool{}
		for _, m := range memberIDs {
			want[m] = true
		}
		var kept []Snapshot
		for _, m := range g.Members {
			if want[m.ID] {
				kept = append(kept, m)
			}
		}
		g.Members = kept
	}
	return g, nil
}

// DeleteGroupSnapshot removes a group snapshot and every snapshot the recursive
// create produced under it.
//
// A member with a dependent clone blocks the whole deletion with
// FailedPrecondition, and the clone is deliberately NOT promoted: promotion
// does not free the snapshot, it inverts the dependency and leaves the SOURCE
// volume undeletable — see DeleteSnapshot for the full reasoning.
func (r *Registry) DeleteGroupSnapshot(ctx context.Context, id string, memberIDs []string) error {
	backendName, groupZFS, err := parseSnapshotID(id)
	if err != nil {
		return err
	}
	if err := validateGroupMembers(id, backendName, groupZFS, memberIDs); err != nil {
		return err
	}
	c, err := r.Client(ctx, backendName)
	if err != nil {
		return err
	}
	found, err := r.groupSnapshots(ctx, c, groupZFS)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return nil // already gone: success, per CSI
	}

	// One dataset listing answers the clone question for every member; asking
	// per member would re-list the pool once per volume in the group.
	parent, _ := splitSnapshotID(groupZFS)
	pool := parent
	if i := strings.Index(pool, "/"); i > 0 {
		pool = pool[:i]
	}
	all, err := c.DatasetList(ctx, pool)
	if err != nil {
		return err
	}
	clonesOf := map[string][]string{}
	for i := range all {
		if o := all[i].Origin.Value; o != "" {
			clonesOf[o] = append(clonesOf[o], all[i].ID)
		}
	}
	var blocked []string
	for _, s := range found {
		blocked = append(blocked, clonesOf[s.ID]...)
	}
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return status.Errorf(codes.FailedPrecondition,
			"group snapshot %s still has dependent volume(s) %v; delete them first", id, blocked)
	}

	// Children first: destroying the anchor before its descendants would leave
	// the members behind if the call failed halfway.
	sort.Slice(found, func(i, j int) bool { return len(found[i].ID) > len(found[j].ID) })
	for _, s := range found {
		if err := c.SnapshotDelete(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

// groupSnapshots returns every snapshot the recursive create made for a group:
// the anchor on the parent dataset plus one per dataset beneath it.
func (r *Registry) groupSnapshots(ctx context.Context, c truenas.API, groupZFS string) ([]truenas.Snapshot, error) {
	parent, name := splitSnapshotID(groupZFS)
	snaps, err := c.SnapshotList(ctx, parent)
	if err != nil {
		return nil, err
	}
	var out []truenas.Snapshot
	for _, s := range snaps {
		if snapshotName(s.ID) != name {
			continue
		}
		ds := s.Dataset
		if ds == "" {
			ds, _ = splitSnapshotID(s.ID)
		}
		if ds != parent && !strings.HasPrefix(ds, parent+"/") {
			continue
		}
		s.Dataset = ds
		out = append(out, s)
	}
	return out, nil
}

// validateGroupMembers rejects a member list that cannot belong to the group,
// which CSI requires whenever the plugin can detect the mismatch.
func validateGroupMembers(groupID, backendName, groupZFS string, memberIDs []string) error {
	parent, name := splitSnapshotID(groupZFS)
	for _, m := range memberIDs {
		mBackend, mZFS, err := parseSnapshotID(m)
		if err != nil {
			return err
		}
		ds, mName := splitSnapshotID(mZFS)
		if mBackend != backendName || mName != name || !strings.HasPrefix(ds, parent+"/") {
			return status.Errorf(codes.InvalidArgument,
				"snapshot %s is not a member of group snapshot %s", m, groupID)
		}
	}
	return nil
}

// splitSnapshotID splits <dataset>@<name>.
func splitSnapshotID(zfsID string) (dataset, name string) {
	if i := strings.LastIndex(zfsID, "@"); i >= 0 {
		return zfsID[:i], zfsID[i+1:]
	}
	return zfsID, ""
}

// snapshotName is the part after the @.
func snapshotName(zfsID string) string {
	_, n := splitSnapshotID(zfsID)
	return n
}

// GroupSnapshotSource turns a CSI group snapshot id into its parts.
func GroupSnapshotSource(id string) (backendName, parentDataset, name string, err error) {
	b, zfsID, err := parseSnapshotID(id)
	if err != nil {
		return "", "", "", err
	}
	ds, n := splitSnapshotID(zfsID)
	if ds == "" || n == "" {
		return "", "", "", status.Errorf(codes.InvalidArgument, "malformed group snapshot id %q", id)
	}
	return b, ds, n, nil
}
