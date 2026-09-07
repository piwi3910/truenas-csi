# truenas-csi — implementation plan

Status: complete
Spec: .procoder/specs/truenas-csi.md

## Goal

Build a production-grade CSI driver that provisions NFS and iSCSI volumes on TrueNAS SCALE
appliances over JSON-RPC 2.0, without ever endangering the pre-existing data on the pool.

## Architecture

A single Go binary runs as either controller or node plugin, selected by a flag. The
controller talks to one or more named TrueNAS appliances through a middleware client that
holds one authenticated websocket per backend, multiplexing requests by JSON-RPC id behind
a bounded concurrency semaphore. Provisioning logic sits behind a `Backend` interface with
`nfs` and `iscsi` implementations, so a third protocol is an added file rather than a
rewrite; the node plugin performs mounts using host binaries it verifies at startup.

## Constraints

Every task inherits these, taken from the spec:

- Go, Apache 2.0, driver name `csi.truenas.watteel.com`, module
  `github.com/pwatteel/truenas-csi`. The driver name is immutable once PVs exist.
- **Transport is wss only.** A URL with scheme http or ws is rejected at config validation
  before any socket is opened — TrueNAS revokes an API key presented over plaintext.
- **Authentication failure is terminal**: log and exit, never retry. Only connection
  failures get backoff.
- Middleware allows at most 20 in-flight calls per connection; the client caps at 16.
- JSON-RPC error `errname` is unreliable (reports EINVAL where the truth is ENOENT), so
  state is established with an explicit query, never inferred from an error.
- The ownership property `io.truenas.csi:managed` must be verified with `source == "LOCAL"`
  before any destructive call, because ZFS user properties are inherited by children.
- Every iscsiadm invocation is scoped to an explicit target and portal. Longhorn shares the
  node's iSCSI stack; no `--logoutall`, no unscoped `-o delete`, no global session rescan.
- Kubernetes 1.31 minimum. arm64 is the primary build and test architecture.
- No credential ever appears in a log line, error, metric label, or process argument.

## Task 1: Repository skeleton and build

Files:

- `go.mod` — module declaration and dependency pins.
- `Makefile` — build, test, lint targets.
- `LICENSE` — Apache 2.0 text.
- `cmd/truenas-csi/main.go` — entrypoint, mode flag only.
- `internal/driver/driver.go` — driver name and version constants.
- `.gitignore` — build output.

Interfaces produced:

- `const DriverName = "csi.truenas.watteel.com"` in `internal/driver`
- `var Version string` in `internal/driver`, set via ldflags
- `func main()` accepting `-mode=controller|node`

Steps:

- [ ] Write `internal/driver/driver_test.go` asserting the driver name is exactly
      `csi.truenas.watteel.com`:
      `func TestDriverName(t *testing.T) { if DriverName != "csi.truenas.watteel.com" { t.Fatalf("got %q", DriverName) } }`
      Run `go test ./internal/driver/` — expect FAIL with "no Go files" (package absent).
- [ ] Create `go.mod` with `module github.com/pwatteel/truenas-csi` and `go 1.24`.
- [ ] Create `internal/driver/driver.go` defining `DriverName` and `Version`.
- [ ] Create `cmd/truenas-csi/main.go` parsing `-mode` and rejecting any value other than
      `controller` or `node` with exit status 1.
- [ ] Add `Makefile` with `build`, `test`, `lint` targets; `test` runs `go test ./...`.
- [ ] Add Apache 2.0 `LICENSE`.
- [ ] Run `go test ./...` — expect PASS. Run `go build ./...` — expect no output.
- [ ] Commit: "build: repository skeleton and driver identity".

## Task 2: Configuration and transport validation

Files:

- `internal/config/config.go` — config types and loader.
- `internal/config/validate.go` — validation rules.
- `internal/config/config_test.go` — tests.

Interfaces produced:

- `type Backend struct { Name, Endpoint, Username, APIKey, Pool, ParentDataset string; CACert []byte; InsecureSkipVerify bool }`
- `type Config struct { Backends map[string]Backend; NodeID string; LogLevel string; MetricsAddr, HealthAddr string }`
- `func Load(path string) (*Config, error)`
- `func (c *Config) Validate() error`
- `var ErrInsecureTransport = errors.New("endpoint must use wss:// — TrueNAS revokes API keys presented over insecure transport")`

Steps:

- [ ] Write `TestRejectsPlaintextEndpoint` in `internal/config/config_test.go`, table-driven
      over `"http://nas/api/current"`, `"ws://nas/api/current"`, `"https://nas"`, each
      expected to return `ErrInsecureTransport`, and `"wss://nas/api/current"` expected to
      pass. Run `go test ./internal/config/` — expect FAIL with "undefined: Validate".
- [ ] Write `TestValidateRequiresBackendFields` asserting a backend missing `Pool` or
      `ParentDataset` fails with a message naming the field.
- [ ] Implement `Backend`, `Config`, and `Load` reading YAML from a path.
- [ ] Implement `Validate`: reject any endpoint whose scheme is not `wss`, reject empty
      `Backends`, reject a backend with an empty `Name`, `Endpoint`, `Username`, `APIKey`,
      `Pool` or `ParentDataset`.
- [ ] Implement `String()` on `Backend` that renders `APIKey` as `[redacted]`, and add
      `TestBackendStringRedactsKey` asserting the literal key text is absent from the output.
- [ ] Run `go test ./internal/config/` — expect PASS.
- [ ] Commit: "config: backend configuration with wss-only transport validation".

## Task 3: Middleware connection and authentication

Files:

- `internal/truenas/client.go` — connection lifecycle.
- `internal/truenas/auth.go` — login.
- `internal/truenas/errors.go` — typed errors.
- `internal/truenas/fake/server.go` — in-process fake middleware for tests.
- `internal/truenas/auth_test.go` — tests.

Interfaces produced:

- `type Client struct{ ... }`
- `func Dial(ctx context.Context, b config.Backend) (*Client, error)`
- `func (c *Client) Close() error`
- `var ErrAuthFailed = errors.New("truenas authentication rejected")`
- `type CallError struct { Code int; ErrName, Reason string }` with `func (e *CallError) Error() string`
- Fake: `func fake.Start(t *testing.T, opts fake.Options) (url string)` where
  `Options{ RejectAuth bool; DropAfter int; ConcurrencyLimit int }`

Steps:

- [ ] Write `internal/truenas/fake/server.go`: a `httptest` TLS server upgrading to
      websocket, answering `auth.login_ex` with `{"response_type":"SUCCESS"}` unless
      `RejectAuth`, and echoing registered method fixtures otherwise.
- [ ] Write `TestAuthFailureIsTerminal`: start the fake with `RejectAuth: true`, call
      `Dial`, assert the returned error wraps `ErrAuthFailed` and assert the fake recorded
      exactly one `auth.login_ex` call. Run `go test ./internal/truenas/` — expect FAIL with
      "undefined: Dial".
- [ ] Implement `Dial`: build a `tls.Config` from `CACert` and `InsecureSkipVerify`, open
      the websocket, send
      `{"jsonrpc":"2.0","id":1,"method":"auth.login_ex","params":[{"mechanism":"API_KEY_PLAIN","username":u,"api_key":k}]}`,
      and return `ErrAuthFailed` when `response_type` is not `SUCCESS`.
- [ ] Implement `CallError` carrying `code`, `data.errname` and `data.reason` from the
      JSON-RPC error object.
- [ ] Run `go test ./internal/truenas/` — expect PASS.
- [ ] Commit: "truenas: websocket client with terminal authentication failure".

## Task 4: Request multiplexing, concurrency cap and reconnect

Files:

- `internal/truenas/mux.go` — reader goroutine and waiter map.
- `internal/truenas/call.go` — `Call` with the in-flight semaphore.
- `internal/truenas/reconnect.go` — backoff loop.
- `internal/truenas/mux_test.go` — tests.

Interfaces produced:

- `func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)`
- `const MaxInFlight = 16`
- `func (c *Client) Notifications() <-chan Notification` for id-less `collection_update`
  messages
- `type Notification struct { Method string; Params json.RawMessage }`

Steps:

- [ ] Write `TestConcurrencyCapUnderLoad`: start the fake with `ConcurrencyLimit: 20`
      (returning JSON-RPC code `-32000` beyond it), issue 100 concurrent `Call`s, assert
      every call succeeds and the fake's observed peak concurrency is at most 16. Run
      `go test ./internal/truenas/ -run TestConcurrencyCapUnderLoad` — expect FAIL with
      "undefined: Call".
- [ ] Write `TestConnectionFailureRetries`: fake with `DropAfter: 1`; assert a second call
      succeeds after reconnect and that `auth.login_ex` was seen twice, once per connection.
- [ ] Write `TestNotificationsAreRouted`: fake emits a `collection_update` with no id;
      assert it arrives on `Notifications()` and does not corrupt an in-flight call.
- [ ] Implement `mux.go`: one reader goroutine; responses with an id resolve the waiting
      future, messages without an id go to the notification channel.
- [ ] Implement `Call`: acquire a weighted semaphore of `MaxInFlight`, assign the next id,
      register a waiter, send, and wait on the context.
- [ ] Implement reconnect with exponential backoff starting at 1s capped at 30s; on
      reconnect re-authenticate once, and if that authentication fails, surface
      `ErrAuthFailed` and stop retrying entirely.
- [ ] Run `go test ./internal/truenas/` — expect PASS.
- [ ] Commit: "truenas: response multiplexing, bounded concurrency, reconnect".

## Task 5: Typed middleware operations and job polling

Files:

- `internal/truenas/dataset.go` — dataset and snapshot operations.
- `internal/truenas/iscsi.go` — iSCSI object operations.
- `internal/truenas/sharing.go` — NFS share operations.
- `internal/truenas/job.go` — job submission and polling.
- `internal/truenas/job_test.go`, `internal/truenas/dataset_test.go` — tests.

Interfaces produced:

- `func (c *Client) DatasetCreate(ctx context.Context, spec DatasetSpec) (*Dataset, error)`
- `func (c *Client) DatasetQuery(ctx context.Context, id string) (*Dataset, error)` returning
  `(nil, nil)` when absent
- `func (c *Client) DatasetUpdate(ctx context.Context, id string, patch map[string]any) (*Dataset, error)`
- `func (c *Client) DatasetDelete(ctx context.Context, id string, recursive, force bool) error`
- `func (c *Client) SnapshotCreate/SnapshotDelete/SnapshotQuery/SnapshotClone(...)`
- `func (c *Client) SetPerm(ctx context.Context, path string, mode string, uid, gid int) error`
- `type Dataset struct { ID, Type, Mountpoint string; VolSize, RefQuota int64; UserProperties map[string]Property }`
- `type Property struct { Value, Source string }`

Steps:

- [ ] Write `TestDatasetQueryAbsentReturnsNil`: fake returns the real observed error shape
      for a missing dataset — code `-32602`, `errname` `EINVAL`, reason
      `"[ENOENT] None: PoolDataset x does not exist"` — and assert `DatasetQuery` returns
      `(nil, nil)` rather than an error, because errname cannot be trusted. Run the test —
      expect FAIL with "undefined: DatasetQuery".
- [ ] Write `TestSetPermPollsJobToCompletion`: fake returns integer job id `9615` from
      `filesystem.setperm`, then reports state `RUNNING` twice and `SUCCESS`; assert
      `SetPerm` returns nil and polled at least three times.
- [ ] Write `TestSetPermFailsOnJobFailure`: fake reports state `FAILED` with an error
      string; assert `SetPerm` returns an error containing that string.
- [ ] Write `TestSetPermTimesOut`: fake never leaves `RUNNING`; assert `SetPerm` returns a
      timeout error within the configured bound rather than blocking forever.
- [ ] Implement the dataset, snapshot, share and iSCSI wrappers, each marshalling the
      documented parameter shapes and unmarshalling into typed structs.
- [ ] Implement `SetPerm`: call `filesystem.setperm`, decode the integer job id, then poll
      `core.get_jobs` with filter `[["id","=",jobID]]` every 300ms until state is one of
      `SUCCESS`, `FAILED`, `ABORTED`, bounded by a 2-minute context.
- [ ] Run `go test ./internal/truenas/` — expect PASS.
- [ ] Commit: "truenas: typed dataset, share, iscsi operations and job polling".

## Task 6: Volume identity and parent-dataset confinement

Files:

- `internal/volume/id.go` — encode and parse.
- `internal/volume/confine.go` — containment check.
- `internal/volume/id_test.go` — tests.

Interfaces produced:

- `type ID struct { Backend, Protocol, Pool, Parent, Name string }`
- `func (id ID) String() string` producing `<backend>/<protocol>/<pool>/<parent>/<name>`
- `func ParseID(s string) (ID, error)`
- `func (id ID) DatasetPath() string` producing `<pool>/<parent>/<name>`
- `func Confine(id ID, allowedPool, allowedParent string) error`
- `var ErrOutsideParent = errors.New("volume resolves outside the configured parent dataset")`

Steps:

- [ ] Write `TestVolumeIDRoundTrip` asserting
      `ParseID("nas1/iscsi/Pool0/k8s/pvc-abc").String()` equals the input and that
      `DatasetPath()` is `Pool0/k8s/pvc-abc`. Run `go test ./internal/volume/` — expect FAIL
      with "undefined: ParseID".
- [ ] Write `TestVolumeIDConfinement`, table-driven, asserting `ErrOutsideParent` for each
      of: `"nas1/nfs/Pool0/k8s/../../Home"`, `"nas1/nfs/Pool0/../Home/x"`,
      `"nas1/nfs/Pool0/k8s/./../../old_homes"`, `"nas1/nfs/OtherPool/k8s/x"`,
      `"nas1/nfs/Pool0/notk8s/x"`, and a name containing `/`; and success for
      `"nas1/nfs/Pool0/k8s/pvc-1"`.
- [ ] Write `TestParseIDRejectsMalformed` covering empty string, three segments, and an
      empty component between separators.
- [ ] Implement `ParseID` splitting into exactly five components, rejecting any component
      that is empty, `.` or `..`, or that contains a path separator after unescaping.
- [ ] Implement `Confine` comparing `filepath.Clean` of the resolved dataset path against
      the configured `<pool>/<parent>` prefix, requiring a separator at the boundary so
      `k8s-other` does not match `k8s`.
- [ ] Run `go test ./internal/volume/` — expect PASS.
- [ ] Commit: "volume: structured identity with parent-dataset confinement".

## Task 7: Ownership marker and delete guard

Files:

- `internal/volume/ownership.go` — stamp and verify.
- `internal/volume/ownership_test.go` — tests.

Interfaces produced:

- `const OwnerProperty = "io.truenas.csi:managed"`
- `const OwnerValue = "truenas-csi"`
- `func StampProperties() []map[string]string` for use in dataset creation
- `func Stamp(ctx context.Context, c *truenas.Client, datasetID string) error` for clones
- `func VerifyOwned(ds *truenas.Dataset) error`
- `var ErrNotManaged = errors.New("dataset is not managed by this driver — refusing to delete")`

Steps:

- [ ] Write `TestDeleteRefusesUnmarkedDataset`, table-driven over a dataset with no
      properties, one whose `io.truenas.csi:managed` has `Source: "INHERITED"`, and one
      whose value is `"something-else"` — each expected to return `ErrNotManaged` — plus one
      with `Value: "truenas-csi", Source: "LOCAL"` expected to return nil. Run
      `go test ./internal/volume/` — expect FAIL with "undefined: VerifyOwned".
- [ ] Implement `VerifyOwned` requiring the property to be present, its value to equal
      `OwnerValue`, and its `Source` to be exactly `LOCAL`.
- [ ] Implement `Stamp` issuing
      `pool.dataset.update <id> {"user_properties_update":[{"key":OwnerProperty,"value":OwnerValue}]}`.
- [ ] Run `go test ./internal/volume/` — expect PASS.
- [ ] Commit: "volume: ownership marker with LOCAL-source delete guard".

## Task 8: Backend interface and multi-appliance registry

Files:

- `internal/backend/backend.go` — the interface every protocol implements.
- `internal/backend/registry.go` — named appliances, one client each.
- `internal/backend/registry_test.go` — tests.

Interfaces produced:

- `type Volume struct { ID volume.ID; CapacityBytes int64; Context map[string]string }`
- `type CreateRequest struct { ID volume.ID; CapacityBytes int64; Params map[string]string; SourceSnapshot string }`
- `type Backend interface { Create(ctx context.Context, r CreateRequest) (*Volume, error); Delete(ctx context.Context, id volume.ID) error; Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error); PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) }`
- `type Registry struct{ ... }`
- `func NewRegistry(ctx context.Context, cfg *config.Config) (*Registry, error)`
- `func (r *Registry) For(name, protocol string) (Backend, error)`
- `func (r *Registry) Client(name string) (*truenas.Client, error)`
- `var ErrUnknownBackend = errors.New("storage class names a backend that is not configured")`

Steps:

- [ ] Write `TestBackendSelectionAndIsolation`: build a registry over two fakes, `nas1`
      healthy and `nas2` refusing connections; assert `For("nas3","nfs")` returns
      `ErrUnknownBackend` with the name in the message, assert a `Create` against `nas1`
      succeeds while `nas2` is down, and assert `nas2`'s failure never blocks `nas1` by
      running both concurrently with a 2-second bound. Run
      `go test ./internal/backend/` — expect FAIL with "undefined: NewRegistry".
- [ ] Write `TestRegistryUnknownProtocol` asserting `For("nas1","smb")` returns an error
      naming `smb` as unsupported in this version.
- [ ] Implement `Registry` holding one `*truenas.Client` per backend, dialled lazily so one
      unreachable appliance does not prevent startup, with its own semaphore per client.
- [ ] Implement `For` dispatching on protocol to the nfs or iscsi implementation bound to
      that backend's client, pool and parent dataset.
- [ ] Run `go test ./internal/backend/` — expect PASS.
- [ ] Commit: "backend: protocol interface and multi-appliance registry".

## Task 9: NFS provisioning backend

Files:

- `internal/backend/nfs/nfs.go` — create, delete, expand, publish context.
- `internal/backend/nfs/nfs_test.go` — tests.

Interfaces produced:

- `func New(c *truenas.Client, pool, parent string) backend.Backend`
- Publish context keys: `server`, `share`, `nfsVersion`
- StorageClass parameters consumed: `nfsVersion` (default `4`), `networks`, `maproot`,
  `mode` (default `0777`), `uid` (default `0`), `gid` (default `0`)

Steps:

- [ ] Write `TestNFSCreateSetsRefquota`: assert the create path issues
      `pool.dataset.create` with `refquota` equal to the requested bytes, and fail the test
      if `refquota` is absent — without it a pod sees the whole pool rather than its volume.
      Run `go test ./internal/backend/nfs/` — expect FAIL with "undefined: New".
- [ ] Write `TestNFSCreateStampsOwnership` asserting `user_properties` carries
      `io.truenas.csi:managed` at creation.
- [ ] Write `TestNFSCreateSetsPermissions` asserting `filesystem.setperm` is called with the
      configured mode, uid and gid before the share is created, because a fresh dataset is
      `root:root 0755` and a non-root pod cannot write to it.
- [ ] Write `TestNFSCreateIsIdempotent`: run `Create` twice with identical parameters and
      assert exactly one `pool.dataset.create` reaches the fake and both calls return the
      same volume — the second must find the existing dataset by query.
- [ ] Write `TestNFSCreateConflictingSize`: existing dataset with a different refquota;
      assert a `codes.AlreadyExists` error.
- [ ] Write `TestNFSDeleteVerifiesOwnership`: fake returns a dataset with no `LOCAL` marker;
      assert `Delete` returns `volume.ErrNotManaged` and issues no `pool.dataset.delete`.
- [ ] Write `TestNFSExpandRejectsShrink` asserting a smaller size returns
      `codes.InvalidArgument` and issues no update, because middleware silently permits a
      refquota shrink below current usage.
- [ ] Implement `Create`: query for an existing dataset first; if absent create it with
      `refquota`, the ownership property and `share_type` unset; then `SetPerm`; then create
      the NFS share with the configured networks and maproot. On any failure after dataset
      creation, delete the dataset before returning so no unmarked partial remains.
- [ ] Implement `Delete`: query the dataset, `VerifyOwned`, delete the NFS share whose path
      matches the mountpoint, then delete the dataset. Return nil when the dataset is
      already absent.
- [ ] Implement `Expand` rejecting any size below the current refquota, then updating it.
- [ ] Implement `PublishContext` returning the server address, export path and nfs version.
- [ ] Run `go test ./internal/backend/nfs/` — expect PASS.
- [ ] Commit: "backend/nfs: dataset, quota, permissions and share provisioning".

## Task 10: iSCSI provisioning backend

Files:

- `internal/backend/iscsi/iscsi.go` — create, delete, expand, publish context.
- `internal/backend/iscsi/target.go` — shared target, portal, CHAP, initiator ACL.
- `internal/backend/iscsi/lun.go` — LUN id allocation.
- `internal/backend/iscsi/iscsi_test.go`, `internal/backend/iscsi/lun_test.go` — tests.

Interfaces produced:

- `func New(c *truenas.Client, pool, parent string) backend.Backend`
- `func ensureTarget(ctx context.Context, c *truenas.Client, p Params) (targetID int, iqn string, err error)`
- `func ensurePortal(ctx context.Context, c *truenas.Client, p Params) (int, error)`
- `func allocateLUN(ctx context.Context, c *truenas.Client, targetID int) (int, error)`
- Publish context keys: `portal`, `iqn`, `lun`, `naa`, `chapUser`, `chapSecretRef`
- StorageClass parameters consumed: `portalID`, `chap` (default `"true"`),
  `initiatorACL` (default `"true"`), `sparse` (default `"true"`), `volblocksize`,
  `fsType` (default `ext4`), `multipath` (default `"false"`)

Steps:

- [ ] Write `TestLUNAllocationUnderConcurrency`: fake tracks created targetextents; run 20
      concurrent `allocateLUN` calls against one target and assert every returned id is
      distinct and contiguous from 0, then simulate a controller restart by discarding all
      in-memory state and assert the next allocation continues from the live query rather
      than restarting at 0. Run `go test ./internal/backend/iscsi/` — expect FAIL with
      "undefined: allocateLUN".
- [ ] Write `TestEnsureTargetIsIdempotent`: two concurrent `ensureTarget` calls produce
      exactly one `iscsi.target.create` at the fake.
- [ ] Write `TestEnsurePortalRespectsOverride`: with `portalID` set, assert no
      `iscsi.portal.create` is issued and the given id is used.
- [ ] Write `TestCHAPGeneratedPerTarget`: assert an `iscsi.auth` entry is created with a
      generated secret of at least 12 characters, that the secret never appears in any log
      line captured during the test, and that `PublishContext` references it without
      inlining it.
- [ ] Write `TestInitiatorACLRestrictsToNodes`: assert the target's initiator group contains
      the supplied node IQNs, and that setting `initiatorACL: "false"` creates no group.
- [ ] Write `TestISCSICreateIsIdempotent` asserting a repeated `Create` yields one zvol and
      one extent, and `TestISCSIDeleteVerifiesOwnership` asserting an unmarked zvol is
      refused.
- [ ] Write `TestISCSIExpandRejectsShrink` asserting a smaller size is rejected by the
      driver before reaching middleware.
- [ ] Implement `allocateLUN`: under the backend lock, query `iscsi.targetextent.query`
      filtered by target, collect used ids, and return the lowest free id — derived from the
      live query every time, never cached.
- [ ] Implement `ensurePortal` and `ensureTarget`: query first, create only when absent, and
      treat a create that fails because the object already exists as success followed by a
      re-query.
- [ ] Implement `Create`: query for an existing zvol; if absent create it with
      `type: VOLUME`, the requested `volsize`, `sparse`, `volblocksize` taken from the
      parameter or `pool.dataset.recommended_zvol_blocksize`, and the ownership property.
      Then create the extent with `type: DISK`, `disk: "zvol/<dataset path>"`, capture the
      returned `naa`, ensure the shared target, allocate a LUN, and create the targetextent.
      Roll back created objects in reverse on any failure.
- [ ] Implement `Delete`: verify ownership on the zvol, then delete targetextent, extent and
      zvol in that order, tolerating each already being absent.
- [ ] Implement `Expand` updating `volsize`, rejecting shrink.
- [ ] Run `go test ./internal/backend/iscsi/` — expect PASS.
- [ ] Commit: "backend/iscsi: zvol, extent, shared target, LUN allocation and CHAP".

## Task 11: Snapshots, clones and restore

Files:

- `internal/backend/snapshot.go` — protocol-independent snapshot operations.
- `internal/backend/snapshot_test.go` — tests.

Interfaces produced:

- `func (r *Registry) CreateSnapshot(ctx context.Context, sourceID volume.ID, name string) (*Snapshot, error)`
- `func (r *Registry) DeleteSnapshot(ctx context.Context, snapshotID string) error`
- `func (r *Registry) ListSnapshots(ctx context.Context, sourceID *volume.ID) ([]Snapshot, error)`
- `type Snapshot struct { ID, SourceVolumeID string; SizeBytes int64; CreationTime time.Time; ReadyToUse bool }`
- `func restoreFromSnapshot(ctx context.Context, c *truenas.Client, snapshotID string, target volume.ID, bytes int64, params map[string]string) error`

Steps:

- [ ] Write `TestRestoredCloneIsManaged`: assert that after `restoreFromSnapshot` the clone
      receives an explicit `user_properties_update` stamping the ownership marker and an
      explicit quota or volsize — a ZFS clone inherits neither from its origin, so without
      both the volume leaks permanently and misreports its size. Run
      `go test ./internal/backend/` — expect FAIL with "undefined: restoreFromSnapshot".
- [ ] Write `TestRestoredNFSCloneGetsPermissions` asserting `filesystem.setperm` runs on a
      restored filesystem volume, because the clone carries the snapshot's permissions
      rather than the new StorageClass's.
- [ ] Write `TestDeleteSnapshotWithDependentClone`: fake reports the snapshot has a clone;
      assert `DeleteSnapshot` returns `codes.FailedPrecondition` and issues no delete.
- [ ] Write `TestSnapshotNeverPromotes`: assert no `pool.dataset.promote` is issued anywhere
      in the restore path — promoting inverts the dependency and makes the SOURCE volume
      undeletable.
- [ ] Implement `CreateSnapshot` calling `pool.snapshot.create` with the source dataset and
      a name derived from the CSI snapshot name, returning `ReadyToUse: true` immediately
      since ZFS snapshots are atomic.
- [ ] Implement `DeleteSnapshot`: query dependent clones first and return
      `FailedPrecondition` when any exist; return nil when the snapshot is already absent.
- [ ] Implement `restoreFromSnapshot`: `pool.snapshot.clone` to the target dataset, then
      stamp ownership, then set refquota or volsize, then for filesystem volumes run
      `SetPerm`, then create the share or iSCSI objects as the protocol requires.
- [ ] Run `go test ./internal/backend/` — expect PASS.
- [ ] Commit: "backend: snapshots, dependency-checked deletion and stamped restore".

## Task 12: CSI Identity and Controller services

Files:

- `internal/csi/identity.go` — Identity service.
- `internal/csi/controller.go` — Controller service.
- `internal/csi/locks.go` — per-volume serialisation.
- `internal/csi/controller_test.go`, `internal/csi/locks_test.go` — tests.

Interfaces produced:

- `func NewIdentity(name, version string) csi.IdentityServer`
- `func NewController(r *backend.Registry, cfg *config.Config) csi.ControllerServer`
- `type VolumeLocks struct{ ... }` with
  `func (l *VolumeLocks) TryAcquire(id string) (release func(), ok bool)`

Steps:

- [ ] Write `TestConcurrentCreateIsIdempotent`: issue 50 concurrent `CreateVolume` calls
      with the same name and assert exactly one `pool.dataset.create` reaches the fake and
      all 50 responses carry the same volume id. Run `go test ./internal/csi/` — expect FAIL
      with "undefined: NewController".
- [ ] Write `TestConcurrentCreateDelete`: interleave `CreateVolume` and `DeleteVolume` for
      one id 100 times and assert the fake ends with either zero or one dataset and no
      orphaned extent, target-extent or share.
- [ ] Write `TestVolumeLocksReturnsAborted` asserting a second concurrent call for the same
      volume id receives `codes.Aborted` so the sidecar retries rather than racing.
- [ ] Write `TestDeleteVolumeAbsentReturnsOK` asserting deleting an already-deleted volume
      returns success, as the CSI spec requires.
- [ ] Write `TestCreateVolumeRejectsUnconfinedID` asserting a request whose resolved dataset
      escapes the parent returns `codes.InvalidArgument` before any middleware call.
- [ ] Implement `VolumeLocks` as a mutex-guarded set of in-flight volume ids.
- [ ] Implement `NewController` wiring `CreateVolume`, `DeleteVolume`,
      `ControllerExpandVolume`, `ValidateVolumeCapabilities`, `ControllerGetCapabilities`,
      `CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots`, each acquiring the volume lock and
      returning `codes.Aborted` when it is held.
- [ ] Implement `NewIdentity` advertising `DriverName`, `Version`, and the
      `CONTROLLER_SERVICE` and `VOLUME_ACCESSIBILITY_CONSTRAINTS` capabilities.
- [ ] Write `TestGracefulShutdownDrainsInFlight`: start the gRPC server, begin a
      `CreateVolume` the fake holds open, signal shutdown, and assert the call completes
      with a real response rather than being cancelled, that no new call is accepted after
      the signal, and that the process exits within the 30-second drain bound.
- [ ] Implement graceful shutdown: on SIGTERM stop accepting new RPCs, wait for in-flight
      calls bounded at 30 seconds, then close each backend client.
- [ ] Run `go test ./internal/csi/` — expect PASS.
- [ ] Commit: "csi: identity and controller services with per-volume locking".

## Task 13: Capacity reporting and volume listing

Files:

- `internal/csi/capacity.go` — GetCapacity and ListVolumes.
- `internal/csi/capacity_test.go` — tests.

Interfaces produced:

- `func (c *controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error)`
- `func (c *controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error)`

Steps:

- [ ] Write `TestCapacityMatchesPool`: fake reports a pool with `free` of 44861949222912;
      assert `GetCapacity` returns exactly that for a StorageClass naming that backend, and
      that a request naming an unknown backend returns `ErrUnknownBackend`. Run
      `go test ./internal/csi/ -run TestCapacityMatchesPool` — expect FAIL with "undefined:
      GetCapacity".
- [ ] Write `TestListVolumesPaginates` asserting a 250-dataset fake returns pages honouring
      `max_entries` and a resumable `next_token`, and that only datasets carrying a `LOCAL`
      ownership marker are listed.
- [ ] Implement `GetCapacity` querying `pool.query` for the named backend and returning
      `available_capacity` from the pool's free bytes.
- [ ] Implement `ListVolumes` querying datasets under the configured parent, filtering to
      owned ones, and paginating by dataset id.
- [ ] Run `go test ./internal/csi/` — expect PASS.
- [ ] Commit: "csi: capacity reporting and owned-volume listing".

## Task 14: Node capability preflight and topology

Files:

- `internal/node/preflight.go` — binary and module detection.
- `internal/node/topology.go` — topology labels from detected capabilities.
- `internal/node/preflight_test.go` — tests.

Interfaces produced:

- `type Capability string` with constants `CapNFS`, `CapISCSI`, `CapXFS`, `CapMultipath`
- `type Preflight struct { Found map[Capability]bool; Missing map[Capability][]string }`
- `func Detect(ctx context.Context, root string) (*Preflight, error)` where `root` is the
  host filesystem root, `/host` in the DaemonSet
- `func (p *Preflight) Require(c Capability) error`
- `func (p *Preflight) TopologyLabels() map[string]string`
- `var ErrCapabilityUnavailable = errors.New("node lacks the tooling this volume requires")`

Steps:

- [ ] Write `TestPreflightMissingTool`: build a temporary fake host root containing
      `sbin/mkfs.ext4` but not `sbin/mkfs.xfs`; assert `Detect` reports `CapXFS` missing
      with `"xfsprogs"` named in `Missing`, and that `Require(CapXFS)` returns an error whose
      message contains `xfsprogs`. Run `go test ./internal/node/` — expect FAIL with
      "undefined: Detect".
- [ ] Write `TestPreflightLoadsModules`: fake root whose `proc/modules` lacks `iscsi_tcp`
      but whose `lib/modules/<rel>/kernel/drivers/scsi/iscsi_tcp.ko` exists; assert `Detect`
      records the module as available and invokes the injected modprobe function exactly
      once, rather than reporting the capability unavailable.
- [ ] Write `TestTopologyLabelsReflectCapabilities` asserting a node without `mkfs.xfs`
      publishes `csi.truenas.watteel.com/xfs="false"` and one with it publishes `"true"`.
- [ ] Implement `Detect` searching `sbin`, `usr/sbin`, `bin`, `usr/bin` under `root` for
      `mount.nfs`, `iscsiadm`, `iscsid`, `mkfs.ext4`, `mkfs.xfs`, `xfs_growfs`, `multipath`,
      `multipathd`, mapping each missing capability to the package that provides it:
      xfs to `xfsprogs`, multipath to `multipath-tools`, iscsi to `open-iscsi`.
- [ ] Implement module detection reading `proc/modules` under `root`, falling back to a
      `.ko` search under `lib/modules/<uname -r>`, and calling modprobe when the object
      exists but is not loaded. Treat available-but-unloaded as recoverable and
      not-present-at-all as unavailable.
- [ ] Implement `TopologyLabels` emitting one label per capability.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: capability preflight, module loading and topology labels".

## Task 15: Node service and NFS mounting

Files:

- `internal/node/node.go` — Node service and capabilities.
- `internal/node/mount.go` — mount helper wrapping the host mounter.
- `internal/node/nfs.go` — NFS stage/unstage/publish/unpublish.
- `internal/node/nfs_test.go` — tests.

Interfaces produced:

- `func NewNode(cfg *config.Config, p *Preflight, exec Executor) csi.NodeServer`
- `type Executor interface { Run(ctx context.Context, name string, args ...string) ([]byte, error) }`
- `func hostExec(root string) Executor` running binaries in the host mount namespace
- `func (n *node) NodeGetInfo(...)` publishing node id, topology labels and
  `max_volumes_per_node`

Steps:

- [ ] Write `TestE2ENFSMountLifecycle` with a recording `Executor`: assert
      `NodeStageVolume` issues exactly
      `mount -t nfs -o vers=4 <server>:<share> <staging path>`, that `nfsVersion: "3"` in the
      publish context changes `vers=4` to `vers=3`, and that `NodeUnstageVolume` issues
      `umount <staging path>` and nothing else. Run `go test ./internal/node/` — expect FAIL
      with "undefined: NewNode".
- [ ] Write `TestNFSStageIsIdempotent` asserting a second `NodeStageVolume` for an
      already-mounted path issues no second mount and returns success.
- [ ] Write `TestNFSUnstageAbsentMountSucceeds` asserting unstaging a path that is not
      mounted returns success and issues no umount.
- [ ] Write `TestNodePublishBindMounts` asserting `NodePublishVolume` bind-mounts the
      staging path to the target path and honours `readonly: true` with the `ro` option.
- [ ] Implement `Executor` with a host-namespace implementation using `nsenter --mount=<root>/proc/1/ns/mnt`.
- [ ] Implement NFS stage as an idempotent mount: check the mount table first, mount only
      when absent, create the staging directory when missing.
- [ ] Implement `NodeGetInfo` returning the configured node id, the topology labels from
      Task 14, and `max_volumes_per_node` of 128.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: node service and idempotent NFS mounting".

## Task 16: Node iSCSI attach, device resolution and raw block

Files:

- `internal/node/iscsi.go` — login, logout, device resolution.
- `internal/node/block.go` — raw block publishing.
- `internal/node/iscsi_test.go` — tests.

Interfaces produced:

- `func resolveDevice(root, naa string) (string, error)` returning the path under
  `/dev/disk/by-id`
- `func iscsiLogin(ctx context.Context, e Executor, portal, iqn string) error`
- `func iscsiLogout(ctx context.Context, e Executor, portal, iqn string) error`
- `var ErrDeviceNotFound = errors.New("iscsi device did not appear after login")`

Steps:

- [ ] Write `TestISCSIDeviceResolution`: build a fake host root containing
      `dev/disk/by-id/scsi-36589cfc000000a960e31390c2657efa7 -> ../../sdc`; assert
      `resolveDevice(root, "0x6589cfc000000a960e31390c2657efa7")` returns that path with the
      `0x` stripped and a `scsi-3` prefix applied, and assert the implementation never lists
      `dev/` itself — scanning races with Longhorn on the same node. Run
      `go test ./internal/node/ -run TestISCSIDeviceResolution` — expect FAIL with
      "undefined: resolveDevice".
- [ ] Write `TestISCSICommandsAreScoped`, a recording `Executor` asserting every issued
      `iscsiadm` command contains both `-T <our iqn>` and `-p <our portal>`, and that none
      contains `--logoutall`, `--op delete` without a target, or `-m session -R`. Longhorn
      sessions must survive.
- [ ] Write `TestE2EUnstageLeavesLonghornIntact`: seed the fake executor with two existing
      Longhorn sessions; run stage then unstage; assert the recorded commands touch only our
      target and that both Longhorn sessions remain in the fake's state.
- [ ] Write `TestBlockVolumeSkipsMkfs` asserting a volume with `volumeMode: Block` is
      bind-mounted to a device file target and no `mkfs.*` command is ever issued.
- [ ] Write `TestFilesystemVolumeFormatsOnce` asserting `mkfs.ext4` runs only when `blkid`
      reports no existing filesystem, and never on a second stage.
- [ ] Write `TestStageFailsWhenFsTypeUnavailable` asserting a request for `xfs` on a
      preflight without `CapXFS` returns an error naming `xfsprogs`, not a mount error.
- [ ] Implement `iscsiLogin` issuing
      `iscsiadm -m discovery -t sendtargets -p <portal>` then
      `iscsiadm -m node -T <iqn> -p <portal> --login`, adding CHAP node options first when
      the publish context carries credentials.
- [ ] Implement `resolveDevice` constructing the by-id path from the NAA and polling for it
      with a 30-second bound, returning `ErrDeviceNotFound` on timeout.
- [ ] Implement staging: resolve device, check `blkid`, format when empty using the
      requested fsType, then mount. For block volumes skip format and mount entirely.
- [ ] Implement `iscsiLogout` issuing `--logout` then `-o delete`, both scoped with `-T` and
      `-p`.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: scoped iscsi attach, deterministic device resolution, raw block".

## Task 17: Node expansion and volume statistics

Files:

- `internal/node/expand.go` — NodeExpandVolume.
- `internal/node/stats.go` — NodeGetVolumeStats.
- `internal/node/expand_test.go`, `internal/node/stats_test.go` — tests.

Interfaces produced:

- `func (n *node) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error)`
- `func (n *node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error)`

Steps:

- [ ] Write `TestE2EExpandMountedISCSI` with a recording `Executor`: assert the node issues
      `iscsiadm -m node -T <iqn> -p <portal> -R` to rescan before growing, then `resize2fs`
      for ext4 or `xfs_growfs` for xfs, and that no umount is issued at any point. Run
      `go test ./internal/node/ -run TestE2EExpandMountedISCSI` — expect FAIL with
      "undefined: NodeExpandVolume".
- [ ] Write `TestExpandNFSIsNoOp` asserting an NFS volume expansion performs no node-side
      command, since the quota change on the appliance is sufficient.
- [ ] Write `TestExpandBlockVolumeIsNoOp` asserting a raw block volume triggers a rescan but
      no filesystem grow.
- [ ] Write `TestE2EVolumeStats` against a temporary directory: assert
      `NodeGetVolumeStats` returns non-zero total and available bytes and an inode entry,
      and that a missing path returns `codes.NotFound`.
- [ ] Implement `NodeExpandVolume`: for iSCSI rescan the session scoped to target and
      portal, wait for the block device size to change with a 30-second bound, then run the
      filesystem-appropriate grow command; for NFS return success without acting.
- [ ] Implement `NodeGetVolumeStats` using `statfs` on the volume path, returning both byte
      and inode usage.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: online expansion via rescan and volume statistics".

## Task 18: Multipath support with single-path degradation

Files:

- `internal/node/multipath.go` — multipath detection and device mapping.
- `internal/node/multipath_test.go` — tests.

Interfaces produced:

- `func multipathDevice(ctx context.Context, e Executor, naa string) (string, bool, error)`
  returning the mapper path and whether multipath is in use

Steps:

- [ ] Write `TestMultipathDegrades`: preflight without `CapMultipath`; assert staging still
      succeeds using the plain by-id device and that a warning containing
      `multipath-tools` is logged exactly once. Run `go test ./internal/node/ -run
      TestMultipathDegrades` — expect FAIL with "undefined: multipathDevice".
- [ ] Write `TestMultipathUsesMapperDevice`: preflight with `CapMultipath` and a fake
      `multipath -l` output naming the NAA; assert the staged device is the
      `/dev/mapper/<wwid>` path rather than the raw `sd` device.
- [ ] Implement `multipathDevice` invoking `multipath -l <wwid>` and parsing the mapper name,
      returning `false` when the capability is absent so the caller falls back.
- [ ] Implement the fallback in the staging path, logging the warning once per node start
      rather than per volume.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: multipath device mapping with single-path fallback".

## Task 19: Observability and secret redaction

Files:

- `internal/obs/metrics.go` — Prometheus collectors.
- `internal/obs/log.go` — structured logging with volume attribution.
- `internal/obs/redact.go` — credential redaction.
- `internal/obs/health.go` — health and readiness endpoints.
- `internal/obs/redact_test.go`, `internal/obs/metrics_test.go` — tests.

Interfaces produced:

- `func ObserveCSI(method string, err error, d time.Duration)`
- `func ObserveMiddleware(method string, err error, d time.Duration)`
- `func Logger(ctx context.Context) *slog.Logger` carrying the volume id when present
- `func WithVolume(ctx context.Context, id string) context.Context`
- `func Redact(s string) string`
- `func Register(secret string)` adding a value to the redaction set

Steps:

- [ ] Write `TestNoSecretsInOutput`: register the API key
      `8-jX9B9ugcrfTfOjY2YdZb01sAuq0RZKAAZp2k24xvrYI67hv3pb8exkJiF8BiAhxz` and a CHAP
      secret, drive a full `CreateVolume` and a failing `CreateVolume` against the fake with
      logs captured to a buffer, and assert neither literal string appears in the buffer, in
      the returned gRPC error text, or in any metric label. Run `go test ./internal/obs/` —
      expect FAIL with "undefined: Redact".
- [ ] Write `TestMetricsAndLogAttribution` asserting that after one `CreateVolume` the
      `truenas_csi_calls_total` counter has one sample with `method="CreateVolume"` and
      `error="false"`, that a middleware counter also incremented, and that every log record
      emitted during the call carries a `volume_id` attribute.
- [ ] Write `TestRedactHandlesSubstrings` asserting a registered secret is masked even when
      embedded in a longer string, and that an empty registration never masks everything.
- [ ] Implement `Redact` over a copy-on-write set of registered secrets, replacing each with
      `[redacted]`, and wire it into the logger and into gRPC error construction.
- [ ] Implement the two histogram-and-counter pairs and a gauge for per-backend connection
      state.
- [ ] Implement `/healthz` returning 200 when the process is up and `/readyz` returning 503
      while no backend has ever connected.
- [ ] Run `go test ./internal/obs/` — expect PASS.
- [ ] Commit: "obs: metrics, volume-attributed logging, credential redaction, health".

## Task 20: Orphan reconciler

Files:

- `internal/reconcile/orphans.go` — the reporting loop.
- `internal/reconcile/orphans_test.go` — tests.

Interfaces produced:

- `func NewOrphanReconciler(r *backend.Registry, lister PVLister, interval time.Duration) *OrphanReconciler`
- `type PVLister interface { VolumeHandles(ctx context.Context) (map[string]struct{}, error) }`
- `func (o *OrphanReconciler) RunOnce(ctx context.Context) (orphans []string, err error)`

Steps:

- [ ] Write `TestOrphanReconcilerReports`: fake holds three owned datasets while the lister
      returns two matching volume handles; assert `RunOnce` returns exactly the third,
      increments `truenas_csi_orphaned_volumes`, logs its id, and — the important assertion
      — that no `pool.dataset.delete` reached the fake. Run `go test ./internal/reconcile/`
      — expect FAIL with "undefined: NewOrphanReconciler".
- [ ] Write `TestOrphanReconcilerIgnoresUnowned` asserting a dataset without a `LOCAL`
      ownership marker is never reported, since it was never ours to begin with.
- [ ] Write `TestOrphanReconcilerSkipsOnListerError` asserting that when the PV lister
      fails, nothing is reported — a partial view must never be read as evidence of orphans.
- [ ] Implement `RunOnce` listing owned datasets per backend, subtracting known PV handles,
      and reporting the remainder through the metric and a warning log.
- [ ] Implement the loop calling `RunOnce` on the interval, defaulting to 30 minutes.
- [ ] Run `go test ./internal/reconcile/` — expect PASS.
- [ ] Commit: "reconcile: report-only orphan detection".

## Task 21: Helm chart

Files:

- `deploy/helm/truenas-csi/Chart.yaml` — chart metadata.
- `deploy/helm/truenas-csi/values.yaml` — defaults.
- `deploy/helm/truenas-csi/templates/controller.yaml` — Deployment with sidecars.
- `deploy/helm/truenas-csi/templates/node.yaml` — privileged DaemonSet.
- `deploy/helm/truenas-csi/templates/rbac.yaml` — ServiceAccounts, Roles, Bindings.
- `deploy/helm/truenas-csi/templates/csidriver.yaml` — CSIDriver object.
- `deploy/helm/truenas-csi/templates/storageclass.yaml` — optional example classes.
- `deploy/helm/truenas-csi/templates/_helpers.tpl` — name helpers.
- `test/chart/chart_test.go` — chart tests.

Interfaces produced:

- Values keys: `backends`, `image.repository`, `image.tag`, `snapshotter.install`,
  `node.kubeletDir` (default `/var/lib/kubelet`), `controller.replicas` (default 2)

Steps:

- [ ] Write `TestChartWithoutSnapshotCRDs`: render the chart with
      `snapshotter.install=false` and no snapshot CRDs present, deploy against a kind
      cluster, and assert the controller becomes Ready, logs a line containing
      `snapshot support disabled`, and omits `CREATE_DELETE_SNAPSHOT` from
      `ControllerGetCapabilities`. Run `go test ./test/chart/` — expect FAIL with "chart
      directory not found".
- [ ] Write `TestChartInstallAndUpgrade`: install the chart on a kind cluster, bind a PVC
      against a fake backend, mount it in a pod, then `helm upgrade` with a changed image
      tag and assert the pod is never evicted and the mount stays readable throughout.
- [ ] Write `TestChartRBACIsMinimal` asserting the controller Role grants no verbs on
      `secrets` beyond `get` on its own named secret, and that the node Role grants no write
      verbs on cluster-scoped resources.
- [ ] Create the controller Deployment with `csi-provisioner`, `csi-attacher`,
      `csi-resizer`, `csi-snapshotter` and `livenessprobe` sidecars, leader election
      enabled, `replicas: 2`, `runAsNonRoot: true`, and a read-only root filesystem.
- [ ] Create the node DaemonSet: privileged, `hostPID: true`, mount propagation
      `Bidirectional` on the kubelet directory, host paths for `/etc/iscsi`,
      `/var/lib/iscsi`, `/dev`, and `/lib/modules` read-only, plus `node-driver-registrar`.
- [ ] Create the `CSIDriver` object with `attachRequired: true`, `podInfoOnMount: true`,
      `storageCapacity: true`, and `fsGroupPolicy: File`.
- [ ] Add a chart note printed on install stating that the snapshot controller is a
      prerequisite unless `snapshotter.install` is set.
- [ ] Run `go test ./test/chart/` — expect PASS.
- [ ] Commit: "deploy: helm chart for controller, node plugin and RBAC".

## Task 22: Release pipeline, signing and SBOM

Files:

- `.github/workflows/ci.yaml` — build, lint, unit and sanity tests.
- `.github/workflows/release.yaml` — multi-arch build, sign, SBOM.
- `test/release/release_test.go` — artifact verification.

Interfaces produced:

- Published image `ghcr.io/pwatteel/truenas-csi:<tag>` for `linux/arm64` and `linux/amd64`

Steps:

- [ ] Write `TestReleaseArtifacts`: for the tag under test, assert `docker manifest inspect`
      lists both `linux/arm64` and `linux/amd64`, that `cosign verify` succeeds against the
      keyless identity of the release workflow, and that an SBOM attestation is present. Run
      `go test ./test/release/` — expect FAIL with "no such image".
- [ ] Write the CI workflow running `go vet`, the linter, unit tests and csi-sanity on every
      push, with arm64 jobs on a native arm64 runner.
- [ ] Write the release workflow building both architectures with buildx, pushing a manifest
      list, signing with cosign keyless, generating an SBOM with syft and attaching it.
- [ ] Pin every GitHub Action to a commit SHA, set explicit `permissions:` blocks, and give
      each job a timeout.
- [ ] Run `go test ./test/release/` against a published pre-release tag — expect PASS.
- [ ] Commit: "ci: multi-arch release with cosign signatures and SBOM".

## Task 23: Conformance and live-appliance integration suite

Files:

- `test/sanity/sanity_test.go` — csi-sanity harness.
- `test/integration/integration_test.go` — live-appliance suite.
- `test/integration/teardown.go` — leak detection.
- `docs/testing.md` — how to run each suite.

Interfaces produced:

- Environment variables `TRUENAS_ENDPOINT`, `TRUENAS_USERNAME`, `TRUENAS_API_KEY`,
  `TRUENAS_POOL`, `TRUENAS_PARENT` for the integration suite
- `func snapshotState(ctx context.Context, c *truenas.Client) (State, error)` and
  `func (s State) Diff(other State) []string`

Steps:

- [ ] Write `TestCSISanity` running the csi-sanity suite against the driver backed by the
      fake middleware, with staging and target paths in `t.TempDir()`. Run
      `go test ./test/sanity/` — expect FAIL with "no such package".
- [ ] Write `TestIntegrationTeardownIsClean`: capture `snapshotState` before the suite —
      dataset ids, NFS shares, iSCSI extents, targets, targetextents, portals — run the full
      integration suite, capture state again, and assert `Diff` is empty. Fail the test with
      the list of surviving objects when it is not.
- [ ] Write `TestE2ENFSProvision`, `TestE2ENFSNonRootWrite`, `TestE2EISCSIProvision`,
      `TestE2ESnapshotRestoreIntegrity` and `TestE2EExpandMountedISCSI` as integration tests
      driving real PVCs against the live appliance, each skipping with a clear message when
      `TRUENAS_ENDPOINT` is unset rather than passing silently.
- [ ] Implement `TestE2ESnapshotRestoreIntegrity` to write a 4 MiB random file, record its
      md5, snapshot, overwrite the source, restore, and assert the restored md5 matches the
      original — the same procedure that validated the design by hand.
- [ ] Implement `TestLeastPrivilegeAccount` running the integration suite against an account
      holding only the 14 documented roles and asserting no operation fails with a
      permission error.
- [ ] Implement the teardown helper so every integration test registers its created objects
      and removes them in reverse order, tolerating already-absent objects.
- [ ] Run `go test ./test/sanity/` — expect PASS. Run the integration suite against the live
      appliance — expect PASS with an empty diff.
- [ ] Commit: "test: csi-sanity conformance and live-appliance integration with leak checks".

## Task 24: Operator documentation

Files:

- `README.md` — overview, installation, StorageClass reference.
- `docs/security.md` — least privilege and the shared-target exposure.
- `docs/troubleshooting.md` — failure modes and their signatures.

Interfaces produced: none; this task documents what earlier tasks built.

Steps:

- [ ] Write `TestDocsListAllStorageClassParameters` in `test/docs/docs_test.go`, parsing the
      parameter names out of the backend implementations and asserting each appears in
      `README.md`; fails when a parameter is added without documentation. Run
      `go test ./test/docs/` — expect FAIL with "README.md not found".
- [ ] Write `docs/security.md` containing the exact 14-role list —
      `DATASET_WRITE`, `DATASET_DELETE`, `POOL_READ`, `SNAPSHOT_WRITE`, `SNAPSHOT_DELETE`,
      `SHARING_ISCSI_EXTENT_WRITE`, `SHARING_ISCSI_TARGET_WRITE`,
      `SHARING_ISCSI_TARGETEXTENT_WRITE`, `SHARING_ISCSI_GLOBAL_READ`,
      `SHARING_ISCSI_PORTAL_READ`, `SHARING_ISCSI_INITIATOR_READ`, `SHARING_ISCSI_AUTH_READ`,
      `SHARING_NFS_WRITE`, `FILESYSTEM_ATTRS_WRITE` — with instructions for creating the
      TrueNAS account, and a prominent section stating that a single shared iSCSI target
      exposes every LUN to every logged-in node, so RWO is not enforced below Kubernetes.
- [ ] Write `docs/troubleshooting.md` covering: an API key revoked by plaintext connection,
      authentication failure being terminal by design, `-32000` backpressure, a StorageClass
      requesting a filesystem the node cannot create, and a PVC stuck Pending because pool
      capacity is exhausted.
- [ ] Write `README.md` covering installation via Helm, the node package prerequisites
      (`open-iscsi`, `xfsprogs`, `cifs-utils`, `nvme-cli`, `multipath-tools`), every
      StorageClass parameter with its default, and the supported protocol matrix.
- [ ] Run `go test ./test/docs/` — expect PASS.
- [ ] Commit: "docs: operator guide, security model and troubleshooting".
