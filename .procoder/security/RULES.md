# Security rules

The repo's rules. A scanner finding is a diagnosis; these decide what to do
with it.

## Blocking, always

- **Any gitleaks finding blocks.** Removing the secret is not the fix — the
  fix is removal _and_ rotation. Assume a credential leaked the moment it was
  written to a file, whether or not it was pushed. This repository has history
  here: a live TrueNAS API key was committed as a test fixture and is still in
  history at `d638270` and `deac44a`.
- **semgrep ERROR blocks.** WARNING and INFO are judged, and the judgement is
  written down — "not exploitable because X", not silence.
- **osv-scanner high/critical blocks.** Upgrade, or record why the path is not
  reachable from this binary.

## What this driver actually holds

An API key with the roles in `docs/security.md`, able to create, expand,
snapshot and destroy datasets and shares beneath its configured parent dataset.
Treat the driver's Secret as storage-administrator access to that subtree.

## Rules specific to this codebase

1. **Plaintext transport is a security bug, not a config error.** TrueNAS
   _revokes_ an API key presented over a plaintext connection. `wss://` and
   `https://` only, in code, tests, scripts, comments and documentation. The
   two `http://` literals in the tree are inside negative tests asserting the
   refusal, and must stay that way.
2. **A credential must never reach a process argument list.** `/proc` makes
   argv readable by every process on the host, and this binary runs on nodes.
   Credentials come from the mounted config or a Secret, never a flag.
3. **Never log a credential.** `obs.Redact` exists for this. The redacting
   handler must be installed on the _default_ logger, not only on `obs`'s —
   that was a real defect once.
4. **Nothing is destroyed that the driver does not own.** Every destructive
   path checks the `io.truenas.csi:managed` user property with
   `source == LOCAL` first. A looser second check elsewhere is a bug, not
   defence in depth.
5. **An access list is never written empty.** On TrueNAS an NFS export with
   both `hosts` and `networks` empty is exported to _everyone_, so "remove the
   node to revoke it" opens the share. One choke point makes that state
   unreachable; tests pin it.
6. **Privilege is deliberate and bounded.** The node plugin is privileged with
   `hostPID`, `hostNetwork` and `/host` mounted `Bidirectional` because it runs
   the host's mount binaries chrooted into the host namespace. Destructive RBAC
   (pods delete, nodes patch, volumeattachments delete) renders **only** when
   the feature needing it is enabled.
7. **Test fixtures must not look like secrets.** Assemble marker-like content
   at runtime rather than as a literal, so our own scanners stay meaningful.

## Judging a finding

Rank by how input reaches it. Data enters through the CSI gRPC surface
(`internal/csi`), the node's publish context, StorageClass parameters, the
appliance's own responses, and Kubernetes objects the controllers read. A
finding on a path no untrusted input reaches is still worth fixing, but it is
not the one to fix first.
