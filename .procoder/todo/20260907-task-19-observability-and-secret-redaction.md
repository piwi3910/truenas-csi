# Task 19: Observability and secret redaction

Status: open
Created: 2026-09-07

## Description

Implements plan task 19 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 19: Observability and secret redaction" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/obs/metrics.go` — Prometheus collectors.
- `internal/obs/log.go` — structured logging with volume attribution.
- `internal/obs/redact.go` — credential redaction.
- `internal/obs/health.go` — health and readiness endpoints.
- `internal/obs/redact_test.go`, `internal/obs/metrics_test.go` — tests.

Interfaces it produces for later tasks:

- `func ObserveCSI(method string, err error, d time.Duration)`
- `func ObserveMiddleware(method string, err error, d time.Duration)`
- `func Logger(ctx context.Context) *slog.Logger` carrying the volume id when present
- `func WithVolume(ctx context.Context, id string) context.Context`
- `func Redact(s string) string`
- `func Register(secret string)` adding a value to the redaction set

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestNoSecretsInOutput, TestMetricsAndLogAttribution, TestRedactHandlesSubstrings.

## Acceptance criteria

- [ ] Write `TestNoSecretsInOutput`: register the API key `8-jX9B9ugcrfTfOjY2YdZb01sAuq0RZKAAZp2k24xvrYI67hv3pb8exkJiF8BiAhxz` and a CHAP secret, drive a full `CreateVolume` and a failing `CreateVolume` against the fake with logs captured to a buffer, and assert neither literal string appears in the buffer, in the returned gRPC error text, or in any metric label. Run `go test ./internal/obs/` — expect FAIL with "undefined: Redact".
- [ ] Write `TestMetricsAndLogAttribution` asserting that after one `CreateVolume` the `truenas_csi_calls_total` counter has one sample with `method="CreateVolume"` and `error="false"`, that a middleware counter also incremented, and that every log record emitted during the call carries a `volume_id` attribute.
- [ ] Write `TestRedactHandlesSubstrings` asserting a registered secret is masked even when embedded in a longer string, and that an empty registration never masks everything.
- [ ] Implement `Redact` over a copy-on-write set of registered secrets, replacing each with `[redacted]`, and wire it into the logger and into gRPC error construction.
- [ ] Implement the two histogram-and-counter pairs and a gauge for per-backend connection state.
- [ ] Implement `/healthz` returning 200 when the process is up and `/readyz` returning 503 while no backend has ever connected.
- [ ] Run `go test ./internal/obs/` — expect PASS.
- [ ] Commit: "obs: metrics, volume-attributed logging, credential redaction, health".

## Evidence

