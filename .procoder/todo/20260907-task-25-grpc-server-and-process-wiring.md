# Task 25: gRPC server and process wiring

Status: closed
Created: 2026-09-07

## Description

Added during implementation. No task in the original plan served the CSI services over a
socket or wired the process together, so the driver could not run and the conformance
suite would have had nothing to exercise. Recorded as plan task 25.

## Acceptance criteria

- [x] Server serves Identity over a UNIX socket and returns the correct plugin name.
- [x] Graceful shutdown drains in-flight calls instead of cancelling them.
- [x] A stale socket left by a crashed plugin does not prevent startup.
- [x] The unary interceptor records metrics and redacts credentials from errors.
- [x] A plaintext endpoint stops the driver before any socket is opened.
- [x] The node adapter persists the publish context so unstage can log out.

## Evidence

- `TestServerServesIdentityOverUnixSocket`, `TestGracefulShutdownDrainsInFlight`,
  `TestServerRemovesStaleSocket`, `TestUnaryInterceptorRecordsMetricsAndRedacts`,
  `TestRunRejectsPlaintextConfig`, `TestLogLevelMapping` all pass.
- Socket paths in tests use a short /tmp directory: macOS caps sun_path at ~104 bytes
  and t.TempDir() alone exceeds it.
- `go test ./...` green; `gofmt -l` and `go vet ./...` clean; gate 0 blocking.
