# Task 2: Configuration and transport validation

Status: closed
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

- [x] Write `TestRejectsPlaintextEndpoint` in `internal/config/config_test.go`, table-driven over `"http://nas/api/current"`, `"ws://nas/api/current"`, `"https://nas"`, each expected to return `ErrInsecureTransport`, and `"wss://nas/api/current"` expected to pass. Run `go test ./internal/config/` — expect FAIL with "undefined: Validate".
- [x] Write `TestValidateRequiresBackendFields` asserting a backend missing `Pool` or `ParentDataset` fails with a message naming the field.
- [x] Implement `Backend`, `Config`, and `Load` reading YAML from a path.
- [x] Implement `Validate`: reject any endpoint whose scheme is not `wss`, reject empty `Backends`, reject a backend with an empty `Name`, `Endpoint`, `Username`, `APIKey`, `Pool` or `ParentDataset`.
- [x] Implement `String()` on `Backend` that renders `APIKey` as `[redacted]`, and add `TestBackendStringRedactsKey` asserting the literal key text is absent from the output.
- [x] Run `go test ./internal/config/` — expect PASS.
- [x] Commit: "config: backend configuration with wss-only transport validation".

## Evidence

- Red first: `go test ./internal/config/` failed with "undefined: Backend" before any source.
- `TestRejectsPlaintextEndpoint` covers http, ws, https, uppercase HTTP, scheme-less and
  empty (all rejected with ErrInsecureTransport) and wss/WSS (accepted).
- `TestValidateRequiresBackendFields` asserts each missing field is named in the error.
- `TestBackendStringRedactsKey` uses a real 66-char key shape and asserts it never appears.
- `TestLoadAppliesDefaultsAndValidates` round-trips YAML, checks defaults, and asserts
  Load rejects an http endpoint.
- Mutation check: replacing the scheme guard with `if false` made 5 assertions fail;
  source restored from snapshot and tests pass again.
- `go test ./internal/config/` ok; `gofmt -l` clean; `go vet ./...` clean;
  `procoder check` 0 blocking.
