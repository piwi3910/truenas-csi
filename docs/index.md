# TrueNAS CSI

A CSI driver for [TrueNAS SCALE](https://www.truenas.com/truenas-scale/): **NFS,
iSCSI, SMB and NVMe/TCP from one binary**, talking to the appliance's JSON-RPC
middleware over `wss://`.

Driver name: `csi.truenas.watteel.com` · Licence: Apache-2.0

---

## What it does

|                     |                                                                                                                                               |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| **Provisioning**    | dynamic and static, thin by default, refquota-backed so a volume's size is enforced rather than advertised                                    |
| **Snapshots**       | CSI snapshots, restore, and clone-from-volume, backed by ZFS snapshots and clones                                                             |
| **Group snapshots** | the upstream CSI `GroupController` service — one recursive ZFS snapshot, so members are crash-consistent at the same transaction group        |
| **Expansion**       | online and offline, controller and node side; shrink is refused because ZFS cannot do it                                                      |
| **Topology**        | nodes publish the protocols and filesystems they can actually serve, and the controller requires them, so an unusable node is never scheduled |
| **Fencing**         | `ControllerUnpublishVolume` revokes appliance-side access, and an opt-in controller acts on it                                                |
| **Replication**     | `StorageProtectionGroup` over ZFS replication, with rehearsal failover                                                                        |
| **Observability**   | array metrics, per-volume I/O counters from node kernel counters, and a Grafana dashboard                                                     |

## Choosing a protocol

Measured on real hardware — see [Performance](performance.md) for the full table
and its caveats.

- **iSCSI** for databases and anything doing small synchronous writes. It is
  roughly **130× faster than NFS** on 4 KiB random writes, because a zvol lets
  the client's own filesystem batch what a server-arbitrated filesystem must
  commit synchronously.
- **NFS** for `ReadWriteMany`. Reads are at the network limit like everything
  else, and shared access is something the block protocols cannot offer at all.
- **NVMe/TCP** where you want block semantics with lower protocol overhead.
- **SMB** for Windows interoperability.

!!! warning "Sequential numbers measure your network"
On the validation cluster every protocol reads at 294 MB/s — one 2.5GbE link
at line rate. That column says nothing about the protocols.

## Getting started

```sh
helm repo add truenas-csi https://piwi3910.github.io/truenas-csi/charts
helm install truenas-csi truenas-csi/truenas-csi \
  --namespace truenas-csi --create-namespace \
  --set backends.nas1.endpoint=wss://nas.example.com/api/current \
  --set backends.nas1.username=truenas_admin \
  --set backends.nas1.apiKey=... \
  --set backends.nas1.pool=tank \
  --set backends.nas1.parentDataset=k8s
```

!!! danger "Always use `wss://`"
TrueNAS **revokes an API key that is presented over a plaintext connection**.
The driver refuses to start on a plaintext endpoint for exactly this reason.
See [Security](security.md).

Install the node packages _before_ the driver first registers on a node —
topology labels are immutable, so a node that gains `open-iscsi` later keeps
advertising that it cannot serve iSCSI. [Troubleshooting](troubleshooting.md)
covers the recovery.

## Where to go next

- [Security model](security.md) — least-privilege roles, TLS trust, the accepted
  shared-target risk, and what the driver refuses to do.
- [Troubleshooting](troubleshooting.md) — failure modes and their signatures.
- [Metrics](metrics.md) — every exported series and what it cannot show.
