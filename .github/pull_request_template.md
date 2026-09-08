## What and why

<!-- The why matters more than the what; the what is in the diff. -->

## How it was verified

<!-- Which suites, and against what. If a live appliance or node was involved,
     say so; if it was only the fake, say that too. -->

- [ ] `go build ./... && go vet ./... && go test ./...`
- [ ] `go test ./test/sanity/`
- [ ] Live appliance / node suite, if the change touches the data path

## Checklist

- [ ] No plaintext endpoint anywhere, including comments and examples
- [ ] Anything unconfirmed against hardware carries an `UNVERIFIED:` comment
      naming the check that would settle it
- [ ] A test that fails if this change is reverted, where the failure would
      otherwise be silent
