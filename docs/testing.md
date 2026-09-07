# Testing

Three suites, in increasing order of what they need.

## Unit tests — no external dependencies

    go test ./...

Everything runs against in-process fakes. `internal/truenas/fake` speaks JSON-RPC 2.0
over a TLS websocket exactly as the appliance does, including its awkward parts: the
in-flight ceiling that answers `-32000`, and the error shape that reports `errname`
`EINVAL` for a missing object whose true errno appears only as an `[ENOENT]` prefix
inside `reason`.

## Conformance — `test/sanity`

    go test ./test/sanity/

Runs the upstream `csi-sanity` suite against Identity, Controller and Node, backed by a
stateful fake appliance and a fake host whose `/proc/mounts` the fake executor actually
maintains — so the node plugin's own "is this already mounted?" checks run against state
that changes. 65 specs pass; the rest are capability-gated and skipped.

## Live appliance — `test/integration`

Needs a real TrueNAS box. Every test skips loudly without it rather than passing silently.

    TRUENAS_ENDPOINT=wss://<host>/api/current \
    TRUENAS_USERNAME=truenas_admin \
    TRUENAS_API_KEY=<key> \
    TRUENAS_POOL=Pool0 \
    TRUENAS_PARENT=csi-integration \
    TRUENAS_DATA_ADDRESS=<host> \
    TRUENAS_INSECURE=true \
    go test -v ./test/integration/

`TRUENAS_ENDPOINT` **must** use `wss://`. A plaintext endpoint makes TrueNAS revoke the
API key outright, and the driver refuses to start with one.

`TRUENAS_PARENT` must already exist as a dataset; the suite creates only children of it.
Point it at a scratch dataset, never at one holding real data.

Set `TRUENAS_LEASTPRIV_API_KEY` to additionally run the suite against an account holding
only the 14 roles in [security.md](security.md).

### Leak detection

`TestIntegrationTeardownIsClean` records every dataset, snapshot, NFS share, iSCSI
extent, target, portal and target-extent before the run and compares afterwards. Anything
left behind fails the test with the list, because on an appliance holding real user data
a leak is the first symptom of a cleanup path that does not work.

Two objects are intentionally kept and reported rather than failed: the **shared iSCSI
target and its portal**. One target per backend serves every volume, so the driver
creates them once and never removes them.
