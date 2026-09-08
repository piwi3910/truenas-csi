package replication

import (
	"context"
	"sort"
	"strconv"

	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// The middleware calls this package needs are declared here rather than in
// internal/truenas, so replication can evolve without widening the shared
// client's surface. Every one of them goes through Client.CallJSON.
const (
	mReplicationQuery  = "replication.query"
	mReplicationCreate = "replication.create"
	mReplicationUpdate = "replication.update"
	mReplicationDelete = "replication.delete"
	mReplicationRun    = "replication.run"

	mSnapshotTaskQuery  = "pool.snapshottask.query"
	mSnapshotTaskCreate = "pool.snapshottask.create"
	mSnapshotTaskUpdate = "pool.snapshottask.update"
	mSnapshotTaskDelete = "pool.snapshottask.delete"

	mSnapshotQuery  = "pool.snapshot.query"
	mSnapshotCreate = "pool.snapshot.create"
	mSnapshotClone  = "pool.snapshot.clone"

	mDatasetQuery   = "pool.dataset.query"
	mDatasetCreate  = "pool.dataset.create"
	mDatasetUpdate  = "pool.dataset.update"
	mDatasetDelete  = "pool.dataset.delete"
	mDatasetPromote = "pool.dataset.promote"
)

// Cron is the schedule shape TrueNAS uses for both periodic snapshot tasks and
// replication tasks.
type Cron struct {
	Minute string `json:"minute,omitempty"`
	Hour   string `json:"hour,omitempty"`
	DOM    string `json:"dom,omitempty"`
	Month  string `json:"month,omitempty"`
	DOW    string `json:"dow,omitempty"`
}

func (c Cron) payload() map[string]any {
	out := map[string]any{
		"minute": firstNonEmpty(c.Minute, "00"),
		"hour":   firstNonEmpty(c.Hour, "*"),
		"dom":    firstNonEmpty(c.DOM, "*"),
		"month":  firstNonEmpty(c.Month, "*"),
		"dow":    firstNonEmpty(c.DOW, "*"),
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// taskState is the {"state": "..."} object a replication task carries.
type taskState struct {
	State string `json:"state"`
	Error string `json:"error"`
}

// ReplicationTask is the subset of a TrueNAS replication task this package uses.
type ReplicationTask struct {
	ID             int       `json:"id"`
	Name           string    `json:"name"`
	Direction      string    `json:"direction"`
	Transport      string    `json:"transport"`
	SourceDatasets []string  `json:"source_datasets"`
	TargetDataset  string    `json:"target_dataset"`
	Enabled        bool      `json:"enabled"`
	Auto           bool      `json:"auto"`
	State          taskState `json:"state"`
}

// snapshotTask is the subset of a periodic snapshot task this package uses.
type snapshotTask struct {
	ID           int    `json:"id"`
	Dataset      string `json:"dataset"`
	NamingSchema string `json:"naming_schema"`
	Enabled      bool   `json:"enabled"`
}

// snapshot is a ZFS snapshot as pool.snapshot.query reports it.
type snapshot struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dataset   string `json:"dataset"`
	CreateTXG string `json:"createtxg"`
}

// txg returns the snapshot's creation transaction group as a number. ZFS
// guarantees it increases monotonically, which makes it the only ordering key
// that is safe across appliances with skewed clocks.
func (s snapshot) txg() int64 {
	n, err := strconv.ParseInt(s.CreateTXG, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// caller is the narrow view of the middleware client this package uses. It
// exists so the test-failover path can wrap a real client in a guard that
// refuses to touch production (see guard.go).
type caller interface {
	CallJSON(ctx context.Context, out any, method string, params ...any) error
}

var _ caller = (*truenas.Client)(nil)

func queryReplicationByName(ctx context.Context, c caller, name string) (*ReplicationTask, error) {
	var out []ReplicationTask
	if err := c.CallJSON(ctx, &out, mReplicationQuery,
		[]any{[]any{"name", "=", name}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func createReplication(ctx context.Context, c caller, payload map[string]any) (*ReplicationTask, error) {
	var out ReplicationTask
	if err := c.CallJSON(ctx, &out, mReplicationCreate, payload); err != nil {
		return nil, err
	}
	return &out, nil
}

func updateReplication(ctx context.Context, c caller, id int, patch map[string]any) error {
	return c.CallJSON(ctx, nil, mReplicationUpdate, id, patch)
}

func deleteReplication(ctx context.Context, c caller, id int) error {
	err := c.CallJSON(ctx, nil, mReplicationDelete, id)
	if err != nil && truenas.IsNotFound(err) {
		return nil
	}
	return err
}

func runReplication(ctx context.Context, c caller, id int) error {
	return c.CallJSON(ctx, nil, mReplicationRun, id)
}

func querySnapshotTask(ctx context.Context, c caller, dataset, schema string) (*snapshotTask, error) {
	var out []snapshotTask
	if err := c.CallJSON(ctx, &out, mSnapshotTaskQuery,
		[]any{[]any{"dataset", "=", dataset}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range out {
		if out[i].NamingSchema == schema {
			return &out[i], nil
		}
	}
	return nil, nil
}

func createSnapshotTask(ctx context.Context, c caller, payload map[string]any) (*snapshotTask, error) {
	var out snapshotTask
	if err := c.CallJSON(ctx, &out, mSnapshotTaskCreate, payload); err != nil {
		return nil, err
	}
	return &out, nil
}

func updateSnapshotTask(ctx context.Context, c caller, id int, patch map[string]any) error {
	return c.CallJSON(ctx, nil, mSnapshotTaskUpdate, id, patch)
}

func deleteSnapshotTask(ctx context.Context, c caller, id int) error {
	err := c.CallJSON(ctx, nil, mSnapshotTaskDelete, id)
	if err != nil && truenas.IsNotFound(err) {
		return nil
	}
	return err
}

// snapshotsOf returns a dataset's snapshots ordered oldest-first by createtxg.
func snapshotsOf(ctx context.Context, c caller, dataset string) ([]snapshot, error) {
	var out []snapshot
	if err := c.CallJSON(ctx, &out, mSnapshotQuery,
		[]any{[]any{"dataset", "=", dataset}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].txg() < out[j].txg() })
	return out, nil
}

// latestSnapshot returns the newest snapshot of a dataset, or nil when it has none.
func latestSnapshot(ctx context.Context, c caller, dataset string) (*snapshot, error) {
	snaps, err := snapshotsOf(ctx, c, dataset)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return &snaps[len(snaps)-1], nil
}

func createSnapshot(ctx context.Context, c caller, dataset, name string) error {
	return c.CallJSON(ctx, nil, mSnapshotCreate, map[string]any{"dataset": dataset, "name": name})
}

func cloneSnapshot(ctx context.Context, c caller, snapshotID, dst string) error {
	return c.CallJSON(ctx, nil, mSnapshotClone,
		map[string]any{"snapshot": snapshotID, "dataset_dst": dst})
}

func queryDataset(ctx context.Context, c caller, id string) (*truenas.Dataset, error) {
	var out []truenas.Dataset
	if err := c.CallJSON(ctx, &out, mDatasetQuery,
		[]any{[]any{"id", "=", id}}, map[string]any{}); err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func createDataset(ctx context.Context, c caller, payload map[string]any) error {
	return c.CallJSON(ctx, nil, mDatasetCreate, payload)
}

func updateDataset(ctx context.Context, c caller, id string, patch map[string]any) error {
	return c.CallJSON(ctx, nil, mDatasetUpdate, id, patch)
}

func deleteDataset(ctx context.Context, c caller, id string) error {
	err := c.CallJSON(ctx, nil, mDatasetDelete, id, map[string]any{"recursive": true, "force": true})
	if err != nil && truenas.IsNotFound(err) {
		return nil
	}
	return err
}

func promoteDataset(ctx context.Context, c caller, id string) error {
	return c.CallJSON(ctx, nil, mDatasetPromote, id)
}

func snapshotShortName(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '@' {
			return id[i+1:]
		}
	}
	return id
}
