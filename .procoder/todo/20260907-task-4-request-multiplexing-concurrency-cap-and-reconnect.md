# Task 4: Request multiplexing, concurrency cap and reconnect

Status: open
Created: 2026-09-07

## Description

Implements plan task 4 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 4: Request multiplexing, concurrency cap and reconnect" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/truenas/mux.go` — reader goroutine and waiter map.
- `internal/truenas/call.go` — `Call` with the in-flight semaphore.
- `internal/truenas/reconnect.go` — backoff loop.
- `internal/truenas/mux_test.go` — tests.

Interfaces it produces for later tasks:

- `func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)`
- `const MaxInFlight = 16`
- `func (c *Client) Notifications() <-chan Notification` for id-less `collection_update`
  messages
- `type Notification struct { Method string; Params json.RawMessage }`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestConcurrencyCapUnderLoad, TestConnectionFailureRetries, TestNotificationsAreRouted.

## Acceptance criteria

- [ ] Write `TestConcurrencyCapUnderLoad`: start the fake with `ConcurrencyLimit: 20` (returning JSON-RPC code `-32000` beyond it), issue 100 concurrent `Call`s, assert every call succeeds and the fake's observed peak concurrency is at most 16. Run `go test ./internal/truenas/ -run TestConcurrencyCapUnderLoad` — expect FAIL with "undefined: Call".
- [ ] Write `TestConnectionFailureRetries`: fake with `DropAfter: 1`; assert a second call succeeds after reconnect and that `auth.login_ex` was seen twice, once per connection.
- [ ] Write `TestNotificationsAreRouted`: fake emits a `collection_update` with no id; assert it arrives on `Notifications()` and does not corrupt an in-flight call.
- [ ] Implement `mux.go`: one reader goroutine; responses with an id resolve the waiting future, messages without an id go to the notification channel.
- [ ] Implement `Call`: acquire a weighted semaphore of `MaxInFlight`, assign the next id, register a waiter, send, and wait on the context.
- [ ] Implement reconnect with exponential backoff starting at 1s capped at 30s; on reconnect re-authenticate once, and if that authentication fails, surface `ErrAuthFailed` and stop retrying entirely.
- [ ] Run `go test ./internal/truenas/` — expect PASS.
- [ ] Commit: "truenas: response multiplexing, bounded concurrency, reconnect".

## Evidence

