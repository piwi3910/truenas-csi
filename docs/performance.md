# Protocol performance

Measured 2026-09-08 through the real driver: provisioned and published with the
controller, mounted on a cluster node the way the node plugin mounts, then fio
from a container against the host mount. The harness is `test/bench` (build tag
`probe`, never part of a normal build or test run).

## Environment

|           |                                                                                  |
| --------- | -------------------------------------------------------------------------------- |
| Appliance | TrueNAS SCALE 25.10.6, pool `Pool0`, 12×6TB RAIDZ2 + 2 SSD, 32 GiB RAM           |
| Node      | worker-22, RK3588 arm64, 8 cores, 31 GiB RAM, kernel 6.12.58                     |
| Network   | `bond0` 5000 Mb/s — **two bonded 2.5GbE links**                                  |
| fio       | 3.36, `direct=1`, 20s runs after a 5s ramp                                       |
| Profile   | 4 KiB random at iodepth 32 for IOPS; 1 MiB sequential at iodepth 8 for bandwidth |

Block volumes (iSCSI, NVMe/TCP) are formatted ext4 and measured through that
filesystem, so all four numbers are filesystem-level and comparable.

## Results

| Protocol | rand read IOPS | rand write IOPS | seq read MB/s | seq write MB/s | read lat |   write lat |
| -------- | -------------: | --------------: | ------------: | -------------: | -------: | ----------: |
| NFS      |         43,272 |             337 |         294.9 |          100.2 |  0.74 ms | **94.6 ms** |
| iSCSI    |         67,453 |          45,155 |         294.6 |          248.3 |  0.47 ms |     0.71 ms |
| SMB      |         66,538 |           1,474 |         294.8 |          157.7 |  0.48 ms |     25.9 ms |
| NVMe/TCP |         46,795 |          31,050 |         294.5 |          161.4 |  0.68 ms |     1.03 ms |

## Reading these numbers honestly

**Every sequential read figure is the network, not the storage.** All four land
within 0.4 MB/s of 294 MB/s, which is 2.5 Gb/s — one link at line rate. The bond
is 5 Gb/s, but a single TCP flow hashes to a single member, so one mount cannot
exceed one link. **This says nothing about the protocols**; it says the test was
bandwidth-bound. Random _reads_ are close to the same wall (67,453 × 4 KiB ≈
263 MB/s), so those are largely bandwidth-bound too.

**The real differentiator is writes**, where the spread is three orders of
magnitude, and it is a difference in _durability semantics_ rather than speed:

- **iSCSI and NVMe/TCP** write to a zvol. The client's own ext4 owns the
  filesystem and batches metadata, so a 4 KiB `O_DIRECT` write is one block
  write the appliance can aggregate.
- **NFS and SMB** are server-arbitrated filesystems. Each `O_DIRECT` write is a
  synchronous round trip that ZFS must commit before acknowledging. 337 IOPS at
  94.6 ms is not a bug — it is a spinning-disk RAIDZ2 pool honouring sync writes
  without a separate log device.

So the ranking depends entirely on the workload:

- **Databases, or anything doing small synchronous writes** → iSCSI, by a wide
  margin. NVMe/TCP is second and closes the gap on larger block sizes.
- **ReadWriteMany, shared config, media, artefacts** → NFS. Its read performance
  is at the network limit like everything else, and RWX is a capability the
  block protocols cannot offer at all.
- **SMB** exists for Windows interoperability. Its write path sits between the
  two, and it is the only protocol here with no session visibility on the
  appliance, so it can never be fenced.

**What would move the write numbers**, in order of effect: an SLOG (a fast
mirrored log device) would transform the NFS and SMB figures, since it is
precisely the sync-write commit that is being measured; `sync=disabled` on the
dataset would do the same and trade durability for it. No volume gets that by
default, and it is never inferred — but it is available per volume, reversibly,
through a `VolumeAttributesClass`: see
[docs/volume-attributes.md](volume-attributes.md) for what it costs and how to
take it back. Multiple parallel flows, or per-mount link pinning, would lift the
294 MB/s read ceiling.

## Reproducing

```sh
TN_EP='wss://<host>/api/current' TN_USER=... TN_KEY=... TN_POOL=Pool0 \
BENCH_PROTOCOLS='nfs,iscsi,smb,nvme' \
BENCH_SMB_USER=... BENCH_SMB_PASS=... \
go run -tags probe ./test/bench
```

It needs the parent dataset `<pool>/csi-bench`, an SMB-enabled account on the
appliance, and a `bench-smb` Secret in the cluster. Volumes are deleted on the
way out.
