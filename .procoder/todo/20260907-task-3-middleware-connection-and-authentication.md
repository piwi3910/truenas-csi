# Task 3: Middleware connection and authentication

Status: open
Created: 2026-09-07

## Description

Implements plan task 3 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 3: Middleware connection and authentication" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/truenas/client.go` — connection lifecycle.
- `internal/truenas/auth.go` — login.
- `internal/truenas/errors.go` — typed errors.
- `internal/truenas/fake/server.go` — in-process fake middleware for tests.
- `internal/truenas/auth_test.go` — tests.

Interfaces it produces for later tasks:

- `type Client struct{ ... }`
- `func Dial(ctx context.Context, b config.Backend) (*Client, error)`
- `func (c *Client) Close() error`
- `var ErrAuthFailed = errors.New("truenas authentication rejected")`
- `type CallError struct { Code int; ErrName, Reason string }` with `func (e *CallError) Error() string`
- Fake: `func fake.Start(t *testing.T, opts fake.Options) (url string)` where
  `Options{ RejectAuth bool; DropAfter int; ConcurrencyLimit int }`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestAuthFailureIsTerminal.

## Acceptance criteria

- [ ] Write `internal/truenas/fake/server.go`: a `httptest` TLS server upgrading to websocket, answering `auth.login_ex` with `{"response_type":"SUCCESS"}` unless `RejectAuth`, and echoing registered method fixtures otherwise.
- [ ] Write `TestAuthFailureIsTerminal`: start the fake with `RejectAuth: true`, call `Dial`, assert the returned error wraps `ErrAuthFailed` and assert the fake recorded exactly one `auth.login_ex` call. Run `go test ./internal/truenas/` — expect FAIL with "undefined: Dial".
- [ ] Implement `Dial`: build a `tls.Config` from `CACert` and `InsecureSkipVerify`, open the websocket, send `{"jsonrpc":"2.0","id":1,"method":"auth.login_ex","params":[{"mechanism":"API_KEY_PLAIN","username":u,"api_key":k}]}`, and return `ErrAuthFailed` when `response_type` is not `SUCCESS`.
- [ ] Implement `CallError` carrying `code`, `data.errname` and `data.reason` from the JSON-RPC error object.
- [ ] Run `go test ./internal/truenas/` — expect PASS.
- [ ] Commit: "truenas: websocket client with terminal authentication failure".

## Evidence

