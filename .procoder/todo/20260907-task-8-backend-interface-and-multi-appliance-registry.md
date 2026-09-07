# Task 8: Backend interface and multi-appliance registry

Status: open
Created: 2026-09-07

## Description

Implements plan task 8 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 8: Backend interface and multi-appliance registry" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/backend/backend.go` — the interface every protocol implements.
- `internal/backend/registry.go` — named appliances, one client each.
- `internal/backend/registry_test.go` — tests.

Interfaces it produces for later tasks:

- `type Volume struct { ID volume.ID; CapacityBytes int64; Context map[string]string }`
- `type CreateRequest struct { ID volume.ID; CapacityBytes int64; Params map[string]string; SourceSnapshot string }`
- `type Backend interface { Create(ctx context.Context, r CreateRequest) (*Volume, error); Delete(ctx context.Context, id volume.ID) error; Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error); PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) }`
- `type Registry struct{ ... }`
- `func NewRegistry(ctx context.Context, cfg *config.Config) (*Registry, error)`
- `func (r *Registry) For(name, protocol string) (Backend, error)`
- `func (r *Registry) Client(name string) (*truenas.Client, error)`
- `var ErrUnknownBackend = errors.New("storage class names a backend that is not configured")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestBackendSelectionAndIsolation, TestRegistryUnknownProtocol.

## Acceptance criteria

- [ ] Write `TestBackendSelectionAndIsolation`: build a registry over two fakes, `nas1` healthy and `nas2` refusing connections; assert `For("nas3","nfs")` returns `ErrUnknownBackend` with the name in the message, assert a `Create` against `nas1` succeeds while `nas2` is down, and assert `nas2`'s failure never blocks `nas1` by running both concurrently with a 2-second bound. Run `go test ./internal/backend/` — expect FAIL with "undefined: NewRegistry".
- [ ] Write `TestRegistryUnknownProtocol` asserting `For("nas1","smb")` returns an error naming `smb` as unsupported in this version.
- [ ] Implement `Registry` holding one `*truenas.Client` per backend, dialled lazily so one unreachable appliance does not prevent startup, with its own semaphore per client.
- [ ] Implement `For` dispatching on protocol to the nfs or iscsi implementation bound to that backend's client, pool and parent dataset.
- [ ] Run `go test ./internal/backend/` — expect PASS.
- [ ] Commit: "backend: protocol interface and multi-appliance registry".

## Evidence

