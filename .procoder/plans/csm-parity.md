# Plan: CSM parity — close the gaps from the Dell analysis

Goal: close every gap named in `.procoder/notes/dell-csm-gap-analysis.md` §6, and
make the resiliency extension a real capability rather than an unconsumed answer.

Architecture: the fence is `ControllerUnpublishVolume` revoking appliance-side
access. iSCSI keeps its single shared target and cluster-wide initiator group;
what moves is the LUN mapping — `iscsi.targetextent` is created at publish and
deleted at unpublish, and because iSCSI here only ever serves SINGLE_NODE_\*
access modes, unmapping fences the one node that had it. NFS and SMB fence by
removing the node's addresses from the share's host access list. A new
controller-side connectivity service answers from the appliance, and a fencing
controller consumes it.

## Constraints (every task inherits these)

- **Transport: `wss://` and `https://` only, never plaintext.** TrueNAS revokes an
  API key that is presented over http. Three keys were destroyed learning this.
  This applies to every tool, script, test and doc example.
- **No live API key is available in this session.** Verify middleware method
  names, parameters and return shapes against the appliance's own published docs
  at `https://192.168.10.253/api/docs/current/` (unauthenticated, https, safe).
  Do not invent a method or a field shape. Anything not confirmed against those
  docs or an existing hardware-verified note must carry an `UNVERIFIED:` comment
  naming what still needs a hardware run.
- `.procoder/notes/truenas-api-findings.md` is authoritative for hardware-verified
  behaviour. Add to it; never contradict it silently.
- Match the existing code's style: comments explain _why_, not _what_; table-driven
  tests; every exported symbol documented; `gofmt` clean.
- Every task ends green: `go build ./... && go vet ./... && go test ./...`.
- Never commit a credential, and never weaken TLS verification by default.

## Interfaces (the contract between tasks)

Task 1 produces, in `internal/truenas`:

    // ReportingQuery is one series requested from reporting.get_data.
    type ReportingQuery struct {
        Name       string    // graph name, e.g. "zfs_dataset"
        Identifier string    // dataset or zvol path; "" for appliance-wide
    }
    type ReportingSeries struct {
        Name       string
        Identifier string
        Legend     []string
        Data       [][]*float64  // rows of [unix_ts, v1, v2...]; nil = gap
    }
    type ISCSISession struct {
        Initiator     string // IQN
        InitiatorAddr string
        Target        string
        TargetName    string
    }
    type NFSClient struct{ Address string }

    // added to the truenas.API interface
    ReportingGetData(ctx context.Context, q []ReportingQuery, start, end time.Time) ([]ReportingSeries, error)
    ISCSISessions(ctx context.Context) ([]ISCSISession, error)
    NFSClients(ctx context.Context) ([]NFSClient, error)

Task 5 produces, in `internal/backend`:

    // NodeRef identifies the node an access grant applies to.
    type NodeRef struct {
        ID    string   // CSI node ID as ControllerPublishVolume receives it
        Addrs []string // node IP addresses, for share host access lists
        IQN   string   // iSCSI initiator name, when known
        NQN   string   // NVMe host NQN, when known
    }

    // Publisher grants and revokes appliance-side access per node. Unpublish is
    // the fence: after it returns the named node must not reach the volume's
    // data, whatever state its kernel is in.
    type Publisher interface {
        Publish(ctx context.Context, id volume.ID, node NodeRef) (map[string]string, error)
        Unpublish(ctx context.Context, id volume.ID, node NodeRef) error
    }

`Backend` keeps `PublishContext` (a read-only accessor with no side effects; it
is used by ControllerGetVolume and the node health monitor). All four backends
implement `Publisher`.

## Tasks

### Wave A — independent

- **Task 1: appliance query surface.** New files in `internal/truenas` only, plus
  the three interface lines in `api.go` and the matching methods on the CORE
  client and `fake`. `reporting.get_data`, `iscsi.global.sessions`, and an NFS
  client list. Do not touch `client.go` or `call.go`.
- **Task 2: operator upgrade gating.** `operator/**` only. A `minUpgradePath` per
  shipped version, refused as a validation error surfaced on the CR's status,
  modelled on csm-operator's `IsValidUpgrade`.
- **Task 3: operational hardening.** Credential hot-reload (fsnotify on the
  mounted Secret, re-dial, never a restart); dynamic log level from a watched
  ConfigMap; a rate limiter and circuit breaker on appliance calls, sized against
  the verified 20-call concurrency ceiling; leader election so one controller
  replica polls array metrics. `internal/config`, `internal/truenas/client.go`,
  `internal/obs`, `cmd`, `deploy/helm`. Do not touch `internal/truenas/api.go`.
- **Task 4: upstream E2E harness.** `test/**` only. Kubernetes external-storage
  E2E with a driver-config manifest, per the direction Dell moved when it retired
  cert-csi.

### Wave B — after Task 1

- **Task 5: the fence primitive.** The `Publisher` interface, all four backends,
  `ControllerPublishVolume`/`ControllerUnpublishVolume`, `PUBLISH_UNPUBLISH_VOLUME`,
  `attachRequired: true`, and the documented CSIDriver-recreation upgrade note.
- **Task 6: per-volume performance metrics — from NODE kernel counters, not the
  appliance.** Verified on hardware 2026-09-08: `reporting.get_data` exposes 40
  graphs and **none** is per-dataset or per-zvol, so the appliance cannot answer
  this. Source the data from `/proc/diskstats` (iSCSI, NVMe) and
  `/proc/self/mountstats` (NFS, SMB) in the node plugin, extending
  `internal/podmon/iocounters.go`, labelled with PVC and namespace, plus a
  Grafana dashboard. Document the two honest limitations: only mounted volumes
  are visible, and a volume's series moves between node exporters when its pod
  reschedules.

### Wave C — after Task 5

- **Task 7: connectivity service and fencing controller.** Move the extension
  controller-side, answering from the appliance; add the consumer that vetoes on
  live sessions or recent I/O, fences with Unpublish, taints, and force-deletes.
- **Task 8: CSI surface completion.** `SINGLE_NODE_MULTI_WRITER`, controller-side
  `GET_VOLUME` and `VOLUME_CONDITION`, `LIST_VOLUMES_PUBLISHED_NODES`, offline
  volume expansion, `MaximumVolumeSize`, ephemeral inline volumes.
- **Task 9: PVC identity.** Record `csi.storage.k8s.io/pvc/name` and
  `/pvc/namespace` — already arriving and currently discarded — as ZFS user
  properties.
- **Task 10: per-namespace capacity accounting.** Optional per-namespace parent
  datasets carrying a ZFS quota, enforced in CreateVolume.
