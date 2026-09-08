# Contributing

## Before you write code

This project keeps a spec and a plan under `.procoder/`, and the notes file
`.procoder/notes/truenas-api-findings.md` is authoritative for **hardware-verified**
appliance behaviour. Read it before assuming anything about the middleware; it
exists because several assumptions in this driver's history turned out to be
wrong in expensive ways.

## The rules that matter

**Never use a plaintext endpoint.** TrueNAS permanently revokes an API key
presented over `http://` or a plaintext WebSocket. Three keys were destroyed
learning this. `wss://` and `https://` only — in code, tests, scripts, comments
and documentation examples. There is a commit gate that will catch you.

**Verify against the appliance, not against the mock.** The appliance's API docs
are served unauthenticated over https at `https://<appliance>/api/docs/current/`,
so a method signature can be checked without presenting a key. Anything not
confirmed there or by a hardware run carries an `UNVERIFIED:` comment naming the
check that would settle it. A fake that is more permissive than the appliance
turns an integration failure into a release failure — when a hardware run finds a
constraint, teach the fake about it in the same change.

**Comments explain why, not what.** The what is in the code underneath.

## Testing

```sh
go build ./... && go vet ./... && go test ./...   # no hardware needed
go test ./test/sanity/                            # csi-sanity conformance
```

Live suites are opt-in and skip cleanly without credentials:

```sh
export TRUENAS_ENDPOINT='wss://nas.example.com/api/current'
export TRUENAS_USERNAME=... TRUENAS_API_KEY=... TRUENAS_POOL=... TRUENAS_PARENT=...
export TRUENAS_E2E_NODE=worker-22   # a real node; without it the node-side tests SKIP
go test ./test/integration/ -v
```

Note that last variable. Without it the suite prints `ok` while silently skipping
every node-side test — read the `--- SKIP` lines, not the summary.

See [docs/testing.md](docs/testing.md) for the full matrix, and `test/external`
for the upstream Kubernetes external-storage conformance harness.

## Pull requests

- One reviewable change per PR, with a message saying _why_.
- Tests that pin the behaviour, and — for anything whose failure would be silent
  — a test that fails if the fix is reverted. There are several of these already;
  `TestLastIOProbeDoesNotUseModTime` exists because a fix silently reverted once.
- `gofmt` clean, CI green on both architectures.
- No attribution or co-author trailers.
