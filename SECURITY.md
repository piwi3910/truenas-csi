# Security policy

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report privately through GitHub's
[security advisories](https://github.com/piwi3910/truenas-csi/security/advisories/new).
Please include the driver version, the TrueNAS SCALE version, the protocol
involved, and what an attacker gains — that last part is what decides urgency.

Expect an acknowledgement within a week. This is a personal project rather than
a vendored product, so there is no paid support and no guaranteed remediation
window; what is offered is an honest assessment and a fix or a documented
mitigation.

## Supported versions

The most recent release. There is no long-term support branch.

## What this driver holds

An API key with the roles listed in [docs/security.md](docs/security.md), which
is enough to create, expand, snapshot and destroy datasets and shares under its
configured parent dataset. Treat the driver's Secret as equivalent to storage
administrator access to that subtree.

## Known, accepted risks

These are design decisions rather than defects, documented so nobody discovers
them the hard way. Each is covered in full in
[docs/security.md](docs/security.md).

- **iSCSI uses one shared target** with a cluster-wide initiator group. A node
  can see LUNs mapped for other nodes while they are mapped. Per-volume fencing
  works by unmapping the LUN, which is sufficient because iSCSI here only serves
  single-node access modes.
- **The node plugin is privileged** with `hostPID`, `hostNetwork` and
  `/host` mounted `Bidirectional`. It runs the host's mount binaries chrooted
  into the host namespace; nothing less will make a mount reach the host.
- **SMB connectivity cannot be observed.** TrueNAS 25.10 publishes no SMB session
  call, so an SMB volume can never satisfy the fencing precondition and an
  SMB-only pod is never force-deleted. Safe direction, stated explicitly.
- **A stock TrueNAS certificate is self-signed** with `SAN=DNS:localhost` and
  cannot be verified against a real address. Supply your own CA bundle rather
  than reaching for `insecureSkipVerify`.

## What the driver refuses to do

- Connect over a plaintext endpoint. TrueNAS revokes an API key presented that
  way, so this is refused at configuration validation, not at first use.
- Delete a dataset it does not own. Every destructive path checks a
  `io.truenas.csi:managed` user property with `source == LOCAL` first.
- Shrink a volume. ZFS cannot, and pretending otherwise loses data.
