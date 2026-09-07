# Task 2: Configuration and transport validation

Status: open
Created: 2026-09-07

## Description

Implements plan task 2 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 2: Configuration and transport validation" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/config/config.go` — config types and loader.
- `internal/config/validate.go` — validation rules.
- `internal/config/config_test.go` — tests.

Interfaces it produces for later tasks:

- `type Backend struct { Name, Endpoint, Username, APIKey, Pool, ParentDataset string; CACert []byte; InsecureSkipVerify bool }`
- `type Config struct { Backends map[string]Backend; NodeID string; LogLevel string; MetricsAddr, HealthAddr string }`
- `func Load(path string) (*Config, error)`
- `func (c *Config) Validate() error`
- `var ErrInsecureTransport = errors.New("endpoint must use wss:// — TrueNAS revokes API keys presented over insecure transport")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestRejectsPlaintextEndpoint, TestValidateRequiresBackendFields, TestBackendStringRedactsKey.

## Acceptance criteria

- [ ] Write `TestRejectsPlaintextEndpoint` in `internal/config/config_test.go`, table-driven over `"http://nas/api/current"`, `"ws://nas/api/current"`, `"https://nas"`, each expected to return `ErrInsecureTransport`, and `"wss://nas/api/current"` expected to pass. Run `go test ./internal/config/` — expect FAIL with "undefined: Validate".
- [ ] Write `TestValidateRequiresBackendFields` asserting a backend missing `Pool` or `ParentDataset` fails with a message naming the field.
- [ ] Implement `Backend`, `Config`, and `Load` reading YAML from a path.
- [ ] Implement `Validate`: reject any endpoint whose scheme is not `wss`, reject empty `Backends`, reject a backend with an empty `Name`, `Endpoint`, `Username`, `APIKey`, `Pool` or `ParentDataset`.
- [ ] Implement `String()` on `Backend` that renders `APIKey` as `[redacted]`, and add `TestBackendStringRedactsKey` asserting the literal key text is absent from the output.
- [ ] Run `go test ./internal/config/` — expect PASS.
- [ ] Commit: "config: backend configuration with wss-only transport validation".

## Evidence

