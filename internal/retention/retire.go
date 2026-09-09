package retention

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Clock is the time source. Tests supply one; production uses time.Now.
type Clock func() time.Time

// Dispose is what a backend calls once a volume's shares, extents and target
// mappings are gone and only the dataset is left.
//
// With delete protection off — which is the default — it calls destroy and
// nothing else, so the code path is byte for byte the one the driver has always
// taken. That is deliberate: a grace period of zero must not mean "a slightly
// different way of destroying the data".
//
// With it on, the dataset is renamed into the graveyard and stamped with when
// it was retired and which volume it was. destroy is not called at all.
//
// The destroy callback exists because the backends do not agree on how a
// dataset is destroyed and must not be made to: the zvol backends retry while
// the appliance reports EBUSY, because removing an extent does not immediately
// release the device, and the filesystem backends do not force. Passing the
// flags instead of the closure would have flattened that into one wrong answer.
func Dispose(ctx context.Context, c truenas.API, p Policy, id volume.ID, destroy func(context.Context) error) error {
	if !p.On() {
		return destroy(ctx)
	}
	return Retire(ctx, c, p, id, time.Now)
}

// Retire renames a volume's dataset into the graveyard and records why.
//
// It must only ever be called after teardown. pool.dataset.rename performs no
// safety checks of its own, so a rename issued while an iSCSI extent or an NFS
// export still points at the dataset would leave that export pointing at a path
// that no longer exists.
func Retire(ctx context.Context, c truenas.API, p Policy, id volume.ID, now Clock) error {
	if !p.On() {
		return fmt.Errorf("retire called with delete protection off")
	}
	// Confine before anything is written. The caller has already confined the
	// id, but this is the last check before a rename that moves data, and a
	// guard that trusts its caller is not a guard.
	if err := volume.Confine(id, p.Pool, p.Parent); err != nil {
		return fmt.Errorf("refusing to retire %s: %w", id, err)
	}
	src := id.DatasetPath()

	if err := ensureGraveyard(ctx, c, p); err != nil {
		return err
	}

	deletedAt := now().UTC()
	dst, err := freeEntry(ctx, c, p, id, deletedAt)
	if err != nil {
		return err
	}

	if err := renameWhenReleased(ctx, c, src, dst); err != nil {
		return fmt.Errorf("retiring %s into %s: %w", src, dst, err)
	}

	// Stamped AFTER the rename, because before it the dataset might not be
	// retired at all. A stamped dataset that was never moved would be a live
	// volume the reaper's timestamp check considers expired — the one ordering
	// that could destroy data still in use.
	//
	// A stamping failure is NOT fatal to the delete: the volume is already
	// retired and gone from the CO's point of view, and failing here would make
	// the CO retry a DeleteVolume that has already succeeded. What it costs is
	// that the dataset has no deletion timestamp, so the reaper will refuse it
	// for ever and an operator must remove it by hand — which is the safe
	// direction for this to fail in, and is why it is logged loudly.
	stamp := map[string]string{
		volume.DeletedAtProperty:   volume.FormatDeletedAt(deletedAt),
		volume.RetiredFromProperty: id.String(),
	}
	for _, key := range []string{volume.DeletedAtProperty, volume.RetiredFromProperty} {
		if err := c.SetUserProperty(ctx, dst, key, stamp[key]); err != nil {
			obs.Logger(ctx).Error("retired volume could not be stamped; the reaper will never "+
				"destroy it and it must be removed by hand once you no longer need it",
				"dataset", dst, "property", key, "error", obs.Redact(err.Error()))
			break
		}
	}

	obs.Logger(ctx).Info("volume retired instead of destroyed: delete protection is on. "+
		"ITS SPACE IS NOT RECLAIMED until the grace period expires — the pool, the pool reserve "+
		"and the capacity reported to the scheduler all still count it as used",
		"volume_id", id.String(), "dataset", src, "retired_to", dst,
		"grace_period", p.Grace.String(),
		"reapable_after", volume.FormatDeletedAt(deletedAt.Add(p.Grace)))
	return nil
}

// ensureGraveyard creates the graveyard dataset if it is missing.
//
// It carries the ownership marker so the driver's existing guards protect it,
// and the graveyard marker so that ListVolumes and the orphan report can tell
// it apart from a volume — it is driver-owned and will never have a
// PersistentVolume, which without the marker is the definition of a permanent
// false positive.
func ensureGraveyard(ctx context.Context, c truenas.API, p Policy) error {
	root := p.Root()
	ds, err := c.DatasetQuery(ctx, root)
	if err != nil {
		return fmt.Errorf("querying graveyard dataset %s: %w", root, err)
	}
	if ds != nil {
		if !volume.IsGraveyard(ds.LocalProperty(volume.GraveyardProperty)) {
			// Something else already lives at this path. Adopting it would put
			// datasets the driver later destroys inside a dataset it did not
			// create, which is the one thing the ownership discipline exists to
			// prevent.
			return fmt.Errorf("dataset %s exists but is not this driver's graveyard "+
				"(no local %s property): choose another deleteProtection.graveyardDataset, "+
				"or move that dataset out of the way", root, volume.GraveyardProperty)
		}
		return nil
	}
	if _, err := c.DatasetCreate(ctx, truenas.DatasetSpec{
		Name: root,
		Type: "FILESYSTEM",
		UserProperties: map[string]string{
			volume.OwnerProperty:     volume.OwnerValue,
			volume.GraveyardProperty: volume.GraveyardValue,
		},
	}); err != nil {
		return fmt.Errorf("creating graveyard dataset %s: %w", root, err)
	}
	obs.Logger(ctx).Info("created the delete-protection graveyard dataset",
		"dataset", root, "note", "retired volumes are moved here and keep occupying pool space "+
			"until their grace period expires")
	return nil
}

// maxEntryName bounds the graveyard entry name.
//
// ZFS_MAX_DATASET_NAME_LEN is 256 for the WHOLE path, and the path here is
// <pool>/<parent>/<graveyard>/<entry>. The volume's real identity is recorded
// in a user property, so truncating the readable part of the name costs
// nothing.
const maxEntryName = 120

// entryName builds the graveyard name for a retired volume: a sortable UTC
// timestamp followed by as much of the volume's dataset leaf as fits.
//
// The timestamp leads so that `zfs list` of the graveyard reads oldest-first,
// which is the order an operator wants when asking what is about to be reaped.
// The leaf is only a convenience — RetiredFromProperty is the identity — so the
// separator is not required to be reversible.
func entryName(id volume.ID, deletedAt time.Time) string {
	leaf := id.Name
	if id.Namespace != "" {
		leaf = id.Namespace + "-" + id.Name
	}
	leaf = sanitiseComponent(leaf)
	name := deletedAt.UTC().Format("20060102T150405Z") + "-" + leaf
	if len(name) > maxEntryName {
		name = name[:maxEntryName]
	}
	return name
}

// sanitiseComponent replaces anything ZFS will not accept in a name component.
//
// The volume's name reaches here from a CSI handle, which permits characters a
// ZFS component does not; substituting rather than rejecting is right because
// uniqueness comes from freeEntry, not from this string.
func sanitiseComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" || out == "." || out == ".." {
		return "volume"
	}
	return out
}

// maxEntryAttempts bounds the search for an unused graveyard name.
const maxEntryAttempts = 32

// freeEntry returns a graveyard path nothing occupies.
//
// A collision needs two volumes retired in the same second whose sanitised
// names match, which will essentially never happen — but "essentially never" is
// not a guarantee, and a rename onto an occupied name is either an error that
// fails the delete or, far worse, an appliance that obliges. The suffix loop
// turns that into a fact.
func freeEntry(ctx context.Context, c truenas.API, p Policy, id volume.ID, deletedAt time.Time) (string, error) {
	base := entryName(id, deletedAt)
	for attempt := 1; attempt <= maxEntryAttempts; attempt++ {
		name := base
		if attempt > 1 {
			name = base + "-" + strconv.Itoa(attempt)
		}
		path := p.Root() + "/" + name
		existing, err := c.DatasetQuery(ctx, path)
		if err != nil {
			return "", fmt.Errorf("querying graveyard entry %s: %w", path, err)
		}
		if existing == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no free name for %s in %s after %d attempts", id, p.Root(), maxEntryAttempts)
}

// renameRetryTimeout bounds how long a retire waits for the kernel to let go of
// a zvol. It matches the zvol delete retry the iSCSI and NVMe-oF backends
// already do, and for the same observed reason: removing an extent does not
// release the device immediately.
var renameRetryTimeout = 30 * time.Second

// renameWhenReleased renames, retrying while the appliance reports the dataset
// busy.
//
// force is passed as TRUE, and the reasoning is worth stating because the
// opposite looks safer and is not. 25.10 refuses EVERY rename without it:
//
//	[EINVAL] pool.dataset.rename.force: No safety checks are performed when
//	renaming ZFS resources; this may break existing usages. If you understand
//	the risks, please set force and proceed.
//
// It is a mandatory acknowledgement, not a conditional safety gate that passes
// when the dataset is idle — verified against the appliance, where a rename of
// a freshly retired dataset with no share, no extent and nothing holding it was
// refused exactly the same way. Passing false does not make the driver careful;
// it makes delete protection fail every single time.
//
// What actually keeps this safe is the ordering, not the flag: DeleteVolume has
// already removed the share, the extent and the target mapping before we get
// here, so by this point there is deliberately nothing left to break. The busy
// retry below covers the one thing teardown cannot make instantaneous — the
// kernel releasing a zvol — and a rename still refused after that is returned,
// not forced past a second time.
func renameWhenReleased(ctx context.Context, c truenas.API, src, dst string) error {
	deadline := time.Now().Add(renameRetryTimeout)
	delay := 200 * time.Millisecond
	for {
		err := c.DatasetRename(ctx, src, dst, true)
		if err == nil {
			return nil
		}
		if !truenas.IsBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}
