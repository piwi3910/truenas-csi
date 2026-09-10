package replication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Appliances is the set of configured TrueNAS appliances. *backend.Registry
// satisfies it; the tests supply a two-appliance stand-in.
//
// Client returns the API interface rather than *truenas.Client so the registry
// really does satisfy this, which it did not: the registry hands out a
// truenas.API, so this interface as written could never be implemented by the
// thing its own comment named. That mismatch is one reason the reconciler was
// never wired into a binary -- it would not have compiled. Replication uses
// only CallJSON, which the interface exposes.
type Appliances interface {
	Client(ctx context.Context, name string) (truenas.API, error)
	Backend(name string) (config.Backend, error)
}

// Group is a StorageProtectionGroup: driver-owned volumes on one appliance,
// replicated to another.
type Group struct {
	// Name identifies the group and names the TrueNAS tasks it owns.
	Name string
	// SourceBackend and TargetBackend are configured appliance names.
	SourceBackend string
	TargetBackend string
	// Volumes are the group's members. Every one must live on SourceBackend and
	// must be driver-owned.
	Volumes []volume.ID
	// SSHCredentialID is the TrueNAS keychain credential (type SSH_CREDENTIALS)
	// describing the target appliance, as configured on the source.
	SSHCredentialID int
	// SSHCredentialIDReverse describes the SOURCE appliance as seen from the
	// target, and is required for failback.
	SSHCredentialIDReverse int
	// Schedule is how often snapshots are taken and replicated.
	Schedule Cron
	// SnapshotRetention is how long source snapshots are kept.
	SnapshotRetention Retention
}

// Retention is a periodic snapshot task's lifetime.
type Retention struct {
	Value int    `json:"value,omitempty"`
	Unit  string `json:"unit,omitempty"` // HOUR DAY WEEK MONTH YEAR
}

func (r Retention) orDefault() Retention {
	if r.Value <= 0 || r.Unit == "" {
		return Retention{Value: 2, Unit: "WEEK"}
	}
	return r
}

// ActionOptions modifies a dangerous action.
type ActionOptions struct {
	// Force overrides a refusal. Every refusal this package makes says what it
	// refused and why, so forcing is a decision an operator makes with the
	// reason in front of them.
	Force bool
}

// DeleteOptions controls group teardown.
type DeleteOptions struct {
	// RemoveTargetDatasets destroys the replicated datasets on the target.
	// Datasets this driver did not create are refused and nothing is deleted.
	RemoveTargetDatasets bool
}

// Status is a group's observable condition.
type Status struct {
	State
	// TaskState is the TrueNAS replication task state (SUCCESS, RUNNING, ...).
	TaskState string
	// TaskError is the middleware's own error text, when the task last failed.
	TaskError string
}

// Manager performs group operations against the configured appliances.
type Manager struct {
	appliances Appliances
	store      Store
}

// NewManager builds a Manager.
func NewManager(a Appliances, s Store) *Manager {
	if s == nil {
		s = NewMemoryStore()
	}
	return &Manager{appliances: a, store: s}
}

// WithStore returns a Manager that records group state somewhere else.
//
// The controller uses it to back state with the custom resource's status, so
// the record that makes failover idempotent survives a controller restart —
// an in-memory record would be lost exactly when it matters most.
func (m *Manager) WithStore(s Store) *Manager {
	if s == nil {
		return m
	}
	return &Manager{appliances: m.appliances, store: s}
}

// Errors this package returns. They are sentinel values because the controller
// must distinguish "refused, and a human must decide" from "try again later".
var (
	// ErrFailoverIncomplete means a previous failover neither finished nor was
	// rolled back. Starting a second one could promote twice.
	ErrFailoverIncomplete = errors.New("a previous failover did not complete")
	// ErrDiverged means the old source changed after failover, so failback
	// would silently discard those changes.
	ErrDiverged = errors.New("source has diverged since failover")
	// ErrWrongPhase means the operation does not apply in the group's phase.
	ErrWrongPhase = errors.New("operation does not apply in this phase")
	// ErrTestFailoverActive means a rehearsal is still running.
	ErrTestFailoverActive = errors.New("a test failover is still active")
	// ErrNoReplicatedSnapshot means the target has nothing to clone or fail over
	// to, which almost always means the first replication has not run yet.
	ErrNoReplicatedSnapshot = errors.New("target has no replicated snapshot")
)

// roots resolves the configured parent dataset on each appliance.
func (m *Manager) roots(g Group) (srcRoot, dstRoot string, err error) {
	srcCfg, err := m.appliances.Backend(g.SourceBackend)
	if err != nil {
		return "", "", err
	}
	dstCfg, err := m.appliances.Backend(g.TargetBackend)
	if err != nil {
		return "", "", err
	}
	return srcCfg.Pool + "/" + srcCfg.ParentDataset, dstCfg.Pool + "/" + dstCfg.ParentDataset, nil
}

func (m *Manager) clients(ctx context.Context, g Group) (src, dst truenas.API, err error) {
	src, err = m.appliances.Client(ctx, g.SourceBackend)
	if err != nil {
		return nil, nil, err
	}
	dst, err = m.appliances.Client(ctx, g.TargetBackend)
	if err != nil {
		return nil, nil, err
	}
	return src, dst, nil
}

// targetDataset is where a member volume lands on the target appliance.
//
// The layout deliberately mirrors the source under the target's own configured
// parent dataset, so the target tree is as confined as the source tree is.
func targetDataset(dstRoot string, id volume.ID) string { return dstRoot + "/" + id.Name }

// VerifyMembership refuses a group that names anything this driver does not own.
//
// This is the first safety property and the reason the rest can be trusted: a
// TrueNAS replication task OVERWRITES its target dataset on every run, so a
// group that names a dataset the driver did not create would replicate over
// somebody's real data — on a schedule, unattended.
func (m *Manager) VerifyMembership(ctx context.Context, g Group) error {
	if len(g.Volumes) == 0 {
		return fmt.Errorf("group %q names no volumes", g.Name)
	}
	srcCfg, err := m.appliances.Backend(g.SourceBackend)
	if err != nil {
		return err
	}
	src, err := m.appliances.Client(ctx, g.SourceBackend)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range g.Volumes {
		if id.Backend != g.SourceBackend {
			return fmt.Errorf("volume %s lives on backend %q, not the group's source %q",
				id, id.Backend, g.SourceBackend)
		}
		// Confinement first: it is a pure check, and it rejects a handle whose
		// components could resolve outside the configured parent dataset before
		// that handle is ever turned into a middleware argument.
		if err := volume.Confine(id, srcCfg.Pool, srcCfg.ParentDataset); err != nil {
			return fmt.Errorf("volume %s: %w", id, err)
		}
		if seen[id.Name] {
			return fmt.Errorf("volume %q appears twice in group %q", id.Name, g.Name)
		}
		seen[id.Name] = true
		if _, err := verifyOwned(ctx, src, id.DatasetPath()); err != nil {
			return fmt.Errorf("group %q member %s: %w", g.Name, id, err)
		}
	}
	return nil
}

// Create makes the group's TrueNAS tasks and is safe to call repeatedly.
//
// It creates one periodic snapshot task per member dataset and one replication
// task bound to them. Binding is what makes the stream consistent: the
// replication task sends exactly the snapshots those tasks produced, rather than
// racing a separate schedule.
func (m *Manager) Create(ctx context.Context, g Group) (*State, error) {
	if err := m.VerifyMembership(ctx, g); err != nil {
		return nil, err
	}
	_, dstRoot, err := m.roots(g)
	if err != nil {
		return nil, err
	}
	src, _, err := m.clients(ctx, g)
	if err != nil {
		return nil, err
	}
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}

	schema := namingSchema(g)
	retention := g.SnapshotRetention.orDefault()
	if st.SnapshotTaskIDs == nil {
		st.SnapshotTaskIDs = map[string]int{}
	}
	sources := make([]string, 0, len(g.Volumes))
	taskIDs := make([]int, 0, len(g.Volumes))
	for _, id := range g.Volumes {
		ds := id.DatasetPath()
		sources = append(sources, ds)

		existing, err := querySnapshotTask(ctx, src, ds, schema)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			created, err := createSnapshotTask(ctx, src, map[string]any{
				"dataset":        ds,
				"recursive":      false,
				"naming_schema":  schema,
				"lifetime_value": retention.Value,
				"lifetime_unit":  retention.Unit,
				"enabled":        true,
				"allow_empty":    true,
				"schedule":       g.Schedule.payload(),
			})
			if err != nil {
				return nil, fmt.Errorf("periodic snapshot task for %s: %w", ds, err)
			}
			existing = created
		}
		st.SnapshotTaskIDs[id.Name] = existing.ID
		taskIDs = append(taskIDs, existing.ID)

		// A baseline snapshot means the first replication run has something to
		// send immediately, instead of the target staying empty — and therefore
		// unusable for failover — until the first cron tick.
		if err := m.ensureBaseline(ctx, src, g, ds); err != nil {
			return nil, err
		}
	}

	name := taskName(g)
	task, err := queryReplicationByName(ctx, src, name)
	if err != nil {
		return nil, err
	}
	if task == nil {
		task, err = createReplication(ctx, src, map[string]any{
			"name":            name,
			"direction":       "PUSH",
			"transport":       "SSH",
			"ssh_credentials": g.SSHCredentialID,
			"source_datasets": sources,
			"target_dataset":  dstRoot,
			"recursive":       false,
			"auto":            true,
			// Bound periodic snapshot tasks cannot also carry a schedule; the
			// window that limits when transfers run is restrict_schedule.
			"periodic_snapshot_tasks": taskIDs,
			"restrict_schedule":       g.Schedule.payload(),
			// SOURCE retention keeps the target a mirror of the source rather
			// than an ever-growing archive nobody prunes.
			"retention_policy": "SOURCE",
			// readonly=SET is what makes the target safe to fail over TO and
			// unsafe to write to before then: the replica cannot be modified
			// until a failover deliberately clears it.
			"readonly": "SET",
			// allow_from_scratch stays false so a mismatched snapshot stream
			// fails loudly instead of destroying the target and re-sending.
			"allow_from_scratch": false,
			"enabled":            true,
		})
		if err != nil {
			return nil, err
		}
		// Seed the target immediately so the group is usable now rather than at
		// the next cron tick.
		if err := runReplication(ctx, src, task.ID); err != nil {
			return nil, fmt.Errorf("initial replication run: %w", err)
		}
	}

	st.TaskID = task.ID
	st.Suspended = !task.Enabled
	if st.Phase == PhaseUnknown {
		st.Phase = PhaseReady
	}
	if st.Suspended && st.Phase == PhaseReady {
		st.Phase = PhaseSuspended
	}
	st.Message = ""
	return st, m.store.Save(ctx, g.Name, st)
}

// ensureBaseline takes the group's baseline snapshot once. Querying first keeps
// Create idempotent: pool.snapshot.create on an existing name is an error.
func (m *Manager) ensureBaseline(ctx context.Context, src caller, g Group, dataset string) error {
	name := "csi-" + g.Name + "-baseline"
	snaps, err := snapshotsOf(ctx, src, dataset)
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if s.Name == name || snapshotShortName(s.ID) == name {
			return nil
		}
	}
	return createSnapshot(ctx, src, dataset, name)
}

// Delete removes the group's TrueNAS tasks.
//
// The replicated data on the target is kept by default: a group is a policy,
// and deleting the policy must not delete the only surviving copy. When the
// operator does ask for the target datasets to go, every one of them is proved
// driver-owned BEFORE anything is destroyed, so a group containing one foreign
// dataset deletes nothing at all.
func (m *Manager) Delete(ctx context.Context, g Group, opts DeleteOptions) error {
	src, dst, err := m.clients(ctx, g)
	if err != nil {
		return err
	}
	_, dstRoot, err := m.roots(g)
	if err != nil {
		return err
	}
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return err
	}
	if st.TestFailover != nil {
		return fmt.Errorf("%w: stop it before deleting group %q", ErrTestFailoverActive, g.Name)
	}

	if opts.RemoveTargetDatasets {
		// Prove ownership of every target dataset first. Interleaving the check
		// with the deletes would leave a half-destroyed group behind when the
		// third dataset turns out to be someone else's.
		for _, id := range g.Volumes {
			target := targetDataset(dstRoot, id)
			ds, err := queryDataset(ctx, dst, target)
			if err != nil {
				return err
			}
			if ds == nil {
				continue
			}
			if err := volume.VerifyOwned(asVolumeDataset(ds)); err != nil {
				return fmt.Errorf("group %q target %s: %w", g.Name, target, err)
			}
		}
	}

	if task, err := queryReplicationByName(ctx, src, taskName(g)); err != nil {
		return err
	} else if task != nil {
		if err := deleteReplication(ctx, src, task.ID); err != nil {
			return err
		}
	}
	schema := namingSchema(g)
	for _, id := range g.Volumes {
		task, err := querySnapshotTask(ctx, src, id.DatasetPath(), schema)
		if err != nil {
			return err
		}
		if task != nil {
			if err := deleteSnapshotTask(ctx, src, task.ID); err != nil {
				return err
			}
		}
	}

	if opts.RemoveTargetDatasets {
		for _, id := range g.Volumes {
			if err := deleteIfOwned(ctx, dst, targetDataset(dstRoot, id)); err != nil {
				return err
			}
		}
	}
	return m.store.Save(ctx, g.Name, &State{Phase: PhaseUnknown})
}

// Status reports the group's phase and its replication task state.
func (m *Manager) Status(ctx context.Context, g Group) (*Status, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	out := &Status{State: *st}
	src, err := m.appliances.Client(ctx, g.SourceBackend)
	if err != nil {
		return out, err
	}
	task, err := queryReplicationByName(ctx, src, taskName(g))
	if err != nil {
		return out, err
	}
	if task == nil {
		out.TaskState = "MISSING"
		return out, nil
	}
	out.TaskState = task.State.State
	out.TaskError = task.State.Error
	out.Suspended = !task.Enabled
	return out, nil
}

// Failover promotes the target appliance to primary.
//
// It is explicit and idempotent, in that order of importance. A group already
// in PhaseFailedOver returns unchanged without issuing a single write, so a
// retrying controller — or an operator running the command twice because the
// first attempt's output scrolled away — cannot promote the target twice.
// A group whose previous failover did NOT finish is refused outright: that
// failover may still be in flight, and two in flight at once is exactly how two
// appliances end up both believing they are primary.
func (m *Manager) Failover(ctx context.Context, g Group, opts ActionOptions) (*State, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	switch st.Phase {
	case PhaseFailedOver:
		return st, nil // already promoted: nothing to do, and nothing to redo
	case PhaseFailingOver:
		if !opts.Force {
			return nil, fmt.Errorf("%w: group %q is in phase %s (%s); "+
				"confirm no promotion is in flight, then force",
				ErrFailoverIncomplete, g.Name, st.Phase, st.Message)
		}
	case PhaseFailingBack:
		if !opts.Force {
			return nil, fmt.Errorf("%w: group %q is failing back", ErrFailoverIncomplete, g.Name)
		}
	}
	if st.TestFailover != nil && !opts.Force {
		return nil, fmt.Errorf("%w: stop it before failing group %q over", ErrTestFailoverActive, g.Name)
	}

	src, dst, err := m.clients(ctx, g)
	if err != nil {
		return nil, err
	}
	_, dstRoot, err := m.roots(g)
	if err != nil {
		return nil, err
	}

	st.Phase = PhaseFailingOver
	st.Message = "promoting " + g.TargetBackend
	if err := m.store.Save(ctx, g.Name, st); err != nil {
		return nil, err
	}

	// Stop the stream first. Promoting the target while the source is still
	// pushing would have replication overwrite the newly writable replica.
	if err := m.setTransfers(ctx, src, g, st, false); err != nil {
		if !opts.Force {
			return nil, fmt.Errorf("group %q: disabling replication on the source: %w "+
				"(if the source appliance is lost, force the failover)", g.Name, err)
		}
	}

	if st.FailoverSnapshots == nil {
		st.FailoverSnapshots = map[string]string{}
	}
	for _, id := range g.Volumes {
		target := targetDataset(dstRoot, id)
		ds, err := verifyOwned(ctx, dst, target)
		if err != nil {
			return nil, fmt.Errorf("group %q target %s: %w", g.Name, target, err)
		}
		latest, err := latestSnapshot(ctx, dst, target)
		if err != nil {
			return nil, err
		}
		if latest == nil && !opts.Force {
			return nil, fmt.Errorf("%w: %s has no replicated snapshot, so failing over would "+
				"promote an empty or unsynchronised replica", ErrNoReplicatedSnapshot, target)
		}
		if latest != nil {
			// The mark failback measures divergence against.
			st.FailoverSnapshots[id.Name] = snapshotShortName(latest.ID)
		}

		// Promotion of a replication target is clearing the readonly the
		// replication task set. Only a dataset that is genuinely a ZFS clone
		// additionally needs pool.dataset.promote.
		if err := updateDataset(ctx, dst, target, map[string]any{"readonly": "OFF"}); err != nil {
			return nil, fmt.Errorf("promoting %s: %w", target, err)
		}
		if ds.OriginSnapshot() != "" {
			if err := promoteDataset(ctx, dst, target); err != nil {
				return nil, fmt.Errorf("promoting clone %s: %w", target, err)
			}
		}
	}

	st.Phase = PhaseFailedOver
	st.Message = g.TargetBackend + " is now primary"
	return st, m.store.Save(ctx, g.Name, st)
}

// setTransfers enables or disables the replication task and its bound snapshot
// tasks, writing only when the appliance disagrees with the wanted value. That
// is what makes Suspend and Resume free to call on every reconcile.
func (m *Manager) setTransfers(ctx context.Context, src caller, g Group, st *State, enabled bool) error {
	task, err := queryReplicationByName(ctx, src, taskName(g))
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("replication task %q does not exist", taskName(g))
	}
	if task.Enabled != enabled {
		if err := updateReplication(ctx, src, task.ID, map[string]any{"enabled": enabled}); err != nil {
			return err
		}
	}
	st.TaskID = task.ID

	schema := namingSchema(g)
	for _, id := range g.Volumes {
		snapTask, err := querySnapshotTask(ctx, src, id.DatasetPath(), schema)
		if err != nil {
			return err
		}
		if snapTask == nil || snapTask.Enabled == enabled {
			continue
		}
		if err := updateSnapshotTask(ctx, src, snapTask.ID, map[string]any{"enabled": enabled}); err != nil {
			return err
		}
	}
	return nil
}

// TestFailover exposes the last replicated snapshot as scratch clones.
//
// Every middleware call it makes goes through scratchGuard, which permits
// writes only inside the group's scratch dataset and refuses promotion and
// every replication-task mutation outright. The production stream therefore
// cannot be disturbed by this path even if the logic below is later changed by
// someone who has not read this comment.
func (m *Manager) TestFailover(ctx context.Context, g Group) (*State, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	if st.TestFailover != nil {
		return st, nil // already running: idempotent
	}
	_, dst, err := m.clients(ctx, g)
	if err != nil {
		return nil, err
	}
	_, dstRoot, err := m.roots(g)
	if err != nil {
		return nil, err
	}

	root := scratchRoot(g, dstRoot)
	guarded := newScratchGuard(dst, root)

	if existing, err := queryDataset(ctx, guarded, root); err != nil {
		return nil, err
	} else if existing == nil {
		if err := createDataset(ctx, guarded, map[string]any{
			"name":            root,
			"type":            "FILESYSTEM",
			"user_properties": volume.StampProperties(root),
		}); err != nil {
			return nil, fmt.Errorf("scratch dataset %s: %w", root, err)
		}
	}

	tf := &TestFailoverState{Root: root, Datasets: map[string]string{}, Snapshots: map[string]string{}}
	for _, id := range g.Volumes {
		target := targetDataset(dstRoot, id)
		latest, err := latestSnapshot(ctx, guarded, target)
		if err != nil {
			return nil, err
		}
		if latest == nil {
			return nil, fmt.Errorf("%w: %s has nothing to rehearse against", ErrNoReplicatedSnapshot, target)
		}
		clone := root + "/" + id.Name
		if existing, err := queryDataset(ctx, guarded, clone); err != nil {
			return nil, err
		} else if existing == nil {
			if err := cloneSnapshot(ctx, guarded, latest.ID, clone); err != nil {
				return nil, fmt.Errorf("cloning %s: %w", latest.ID, err)
			}
		}
		// A ZFS clone inherits neither the ownership marker nor the quota, so
		// an unstamped scratch clone could never be cleaned up by the guard
		// that tears this down again.
		if err := updateDataset(ctx, guarded, clone, map[string]any{
			"user_properties_update": volume.StampProperties(clone),
		}); err != nil {
			return nil, fmt.Errorf("stamping %s: %w", clone, err)
		}
		tf.Datasets[id.Name] = clone
		tf.Snapshots[id.Name] = snapshotShortName(latest.ID)
	}

	st.TestFailover = tf
	return st, m.store.Save(ctx, g.Name, st)
}

// StopTestFailover destroys the scratch clones.
func (m *Manager) StopTestFailover(ctx context.Context, g Group) (*State, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	if st.TestFailover == nil {
		return st, nil
	}
	_, dst, err := m.clients(ctx, g)
	if err != nil {
		return nil, err
	}
	guarded := newScratchGuard(dst, st.TestFailover.Root)

	names := make([]string, 0, len(st.TestFailover.Datasets))
	for _, ds := range st.TestFailover.Datasets {
		names = append(names, ds)
	}
	sort.Strings(names)
	for _, ds := range names {
		if err := deleteIfOwned(ctx, guarded, ds); err != nil {
			return nil, fmt.Errorf("test failover clone %s: %w", ds, err)
		}
	}
	if err := deleteIfOwned(ctx, guarded, st.TestFailover.Root); err != nil {
		return nil, fmt.Errorf("test failover scratch dataset %s: %w", st.TestFailover.Root, err)
	}

	st.TestFailover = nil
	return st, m.store.Save(ctx, g.Name, st)
}

// divergence lists, per volume, the snapshots the old source gained after the
// failover mark. Reverse replication would roll the source back past them.
func (m *Manager) divergence(ctx context.Context, src caller, g Group, st *State) (map[string][]string, error) {
	out := map[string][]string{}
	for _, id := range g.Volumes {
		mark, ok := st.FailoverSnapshots[id.Name]
		if !ok {
			// No mark means failover never recorded a common point for this
			// volume, so nothing can be proved about it. Treat that as
			// divergence: refusing is recoverable, a silent rollback is not.
			out[id.Name] = []string{"no common snapshot was recorded at failover"}
			continue
		}
		snaps, err := snapshotsOf(ctx, src, id.DatasetPath())
		if err != nil {
			return nil, err
		}
		idx := -1
		for i, s := range snaps {
			if snapshotShortName(s.ID) == mark {
				idx = i
				break
			}
		}
		if idx < 0 {
			out[id.Name] = []string{fmt.Sprintf("the common snapshot %q is gone from the source", mark)}
			continue
		}
		for _, s := range snaps[idx+1:] {
			out[id.Name] = append(out[id.Name], snapshotShortName(s.ID))
		}
	}
	for k, v := range out {
		if len(v) == 0 {
			delete(out, k)
		}
	}
	return out, nil
}

// Failback returns primacy to the source appliance.
//
// It refuses when the old source has changed since failover, because failback
// is a reverse replication and reverse replication rolls the source back to the
// target's snapshot stream: anything written to the old source in the meantime
// is destroyed. The refusal names every volume and every snapshot involved, so
// forcing it is an informed choice rather than a shrug.
func (m *Manager) Failback(ctx context.Context, g Group, opts ActionOptions) (*State, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	switch st.Phase {
	case PhaseReady, PhaseSuspended:
		return st, nil // already primary here: idempotent
	case PhaseFailingBack:
		if !opts.Force {
			return nil, fmt.Errorf("%w: group %q is already failing back", ErrFailoverIncomplete, g.Name)
		}
	case PhaseFailedOver:
	default:
		return nil, fmt.Errorf("%w: group %q is in phase %s", ErrWrongPhase, g.Name, st.Phase)
	}
	if st.TestFailover != nil && !opts.Force {
		return nil, fmt.Errorf("%w: stop it before failing group %q back", ErrTestFailoverActive, g.Name)
	}

	src, dst, err := m.clients(ctx, g)
	if err != nil {
		return nil, err
	}
	srcRoot, dstRoot, err := m.roots(g)
	if err != nil {
		return nil, err
	}

	diverged, err := m.divergence(ctx, src, g, st)
	if err != nil {
		return nil, err
	}
	if len(diverged) > 0 && !opts.Force {
		return nil, fmt.Errorf("%w: %s; failing back would discard this. "+
			"Force only if these changes are expendable", ErrDiverged, describeDivergence(diverged))
	}

	st.Phase = PhaseFailingBack
	st.Message = "replicating " + g.TargetBackend + " back to " + g.SourceBackend
	if err := m.store.Save(ctx, g.Name, st); err != nil {
		return nil, err
	}

	// The reverse task lives on the TARGET, which is currently primary, and
	// pushes back to the source. It is temporary: leaving it behind would give
	// the group two tasks that could both run.
	sources := make([]string, 0, len(g.Volumes))
	for _, id := range g.Volumes {
		sources = append(sources, targetDataset(dstRoot, id))
	}
	reverse, err := queryReplicationByName(ctx, dst, reverseTaskName(g))
	if err != nil {
		return nil, err
	}
	if reverse == nil {
		reverse, err = createReplication(ctx, dst, map[string]any{
			"name":             reverseTaskName(g),
			"direction":        "PUSH",
			"transport":        "SSH",
			"ssh_credentials":  g.SSHCredentialIDReverse,
			"source_datasets":  sources,
			"target_dataset":   srcRoot,
			"recursive":        false,
			"auto":             false,
			"retention_policy": "NONE",
			"readonly":         "IGNORE",
			// Forcing a failback over a diverged source is exactly the case
			// where the source's snapshot stream no longer matches; only then
			// may replication start it again from scratch.
			"allow_from_scratch": opts.Force,
			"enabled":            true,
		})
		if err != nil {
			return nil, fmt.Errorf("reverse replication task: %w", err)
		}
	}
	if err := runReplication(ctx, dst, reverse.ID); err != nil {
		return nil, fmt.Errorf("reverse replication run: %w", err)
	}
	if err := deleteReplication(ctx, dst, reverse.ID); err != nil {
		return nil, err
	}

	// The source is primary again: writable, with its own stream running.
	for _, id := range g.Volumes {
		if err := updateDataset(ctx, src, id.DatasetPath(), map[string]any{"readonly": "OFF"}); err != nil {
			return nil, err
		}
	}
	if err := m.setTransfers(ctx, src, g, st, true); err != nil {
		return nil, err
	}

	st.Phase = PhaseReady
	st.Suspended = false
	st.FailoverSnapshots = nil
	st.Message = g.SourceBackend + " is primary again"
	return st, m.store.Save(ctx, g.Name, st)
}

func describeDivergence(d map[string][]string) string {
	names := make([]string, 0, len(d))
	for k := range d {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s gained %s", n, strings.Join(d[n], ", ")))
	}
	return strings.Join(parts, "; ")
}

// Suspend stops replication transfers without destroying anything.
func (m *Manager) Suspend(ctx context.Context, g Group) (*State, error) {
	return m.setSuspended(ctx, g, true)
}

// Resume restarts replication transfers.
func (m *Manager) Resume(ctx context.Context, g Group) (*State, error) {
	return m.setSuspended(ctx, g, false)
}

// setSuspended is level-triggered: it reads the appliance, writes only if the
// appliance disagrees, and therefore costs nothing to call on every reconcile.
func (m *Manager) setSuspended(ctx context.Context, g Group, suspended bool) (*State, error) {
	st, err := m.store.Load(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	if st.Phase == PhaseFailedOver || st.Phase == PhaseFailingOver {
		return nil, fmt.Errorf("%w: group %q is failed over; there is no source stream to %s",
			ErrWrongPhase, g.Name, map[bool]string{true: "suspend", false: "resume"}[suspended])
	}
	src, err := m.appliances.Client(ctx, g.SourceBackend)
	if err != nil {
		return nil, err
	}
	if err := m.setTransfers(ctx, src, g, st, !suspended); err != nil {
		return nil, err
	}
	st.Suspended = suspended
	if suspended {
		st.Phase = PhaseSuspended
		st.Message = "replication transfers are disabled"
	} else {
		st.Phase = PhaseReady
		st.Message = ""
	}
	return st, m.store.Save(ctx, g.Name, st)
}

// CreateRemoteVolume provisions the target-side dataset for one member volume.
//
// The remote dataset is stamped with the ownership marker at creation. Without
// it the driver's own guard would refuse to remove the dataset later, so an
// unstamped remote volume is a permanent leak on the target appliance — and,
// worse, indistinguishable from an operator's own dataset.
func (m *Manager) CreateRemoteVolume(ctx context.Context, g Group, id volume.ID) (string, error) {
	srcCfg, err := m.appliances.Backend(g.SourceBackend)
	if err != nil {
		return "", err
	}
	if err := volume.Confine(id, srcCfg.Pool, srcCfg.ParentDataset); err != nil {
		return "", fmt.Errorf("volume %s: %w", id, err)
	}
	src, dst, err := m.clients(ctx, g)
	if err != nil {
		return "", err
	}
	_, dstRoot, err := m.roots(g)
	if err != nil {
		return "", err
	}

	// The source volume must be one of ours: the remote dataset it creates is
	// what a later replication task will overwrite.
	source, err := verifyOwned(ctx, src, id.DatasetPath())
	if err != nil {
		return "", err
	}

	target := targetDataset(dstRoot, id)
	existing, err := queryDataset(ctx, dst, target)
	if err != nil {
		return "", err
	}
	if existing != nil {
		// Idempotent, but only for a dataset this driver owns. Adopting a
		// stranger's dataset as a replication target would destroy it.
		if err := volume.VerifyOwned(asVolumeDataset(existing)); err != nil {
			return "", fmt.Errorf("remote volume %s already exists: %w", target, err)
		}
		return target, nil
	}

	payload := map[string]any{
		"name":            target,
		"type":            source.Type,
		"user_properties": volume.StampProperties(target),
	}
	if source.Type == "VOLUME" {
		payload["volsize"] = source.VolSize.Parsed
		payload["sparse"] = true
	} else {
		payload["type"] = "FILESYSTEM"
		if q := source.RefQuota.Parsed; q > 0 {
			payload["refquota"] = q
		}
	}
	if err := createDataset(ctx, dst, payload); err != nil {
		return "", fmt.Errorf("remote volume %s: %w", target, err)
	}
	return target, nil
}

// taskName is the replication task's name on the appliance. It carries the
// driver's prefix so an operator can tell at a glance which tasks in the
// TrueNAS UI belong to Kubernetes.
func taskName(g Group) string { return "csi-" + g.Name }

// reverseTaskName is the temporary task that pushes the target back to the source.
func reverseTaskName(g Group) string { return "csi-" + g.Name + "-failback" }

// namingSchema is the snapshot name template the periodic snapshot tasks use.
// The replication task selects snapshots by it, so it must not overlap with any
// other task's schema.
func namingSchema(g Group) string { return "csi-" + g.Name + "-%Y-%m-%d_%H-%M" }

// scratchRoot is the parent dataset a test failover clones into. Every write a
// test failover is permitted to make is confined to this subtree.
func scratchRoot(g Group, targetRoot string) string {
	return targetRoot + "/csi-testfailover-" + g.Name
}
