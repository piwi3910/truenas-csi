// Package obs provides observability for the driver: Prometheus metrics,
// volume-attributed structured logging, credential redaction and health endpoints.
//
// No credential ever appears in a log line, an error message or a metric label. Every
// value passed to Register is masked in log output and in errors built through RedactErr,
// and metric labels only ever carry method and backend names.
package obs

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Mask replaces a registered secret wherever it appears.
const Mask = "[redacted]"

// minSecretLen is the shortest value worth registering. A one or two character
// registration would mask ordinary text everywhere, destroying the logs it is meant to
// protect, so such values are ignored. Real credentials (API keys, CHAP secrets) are far
// longer than this bound.
const minSecretLen = 6

var (
	// registerMu serialises writers; readers use the atomic pointer below.
	registerMu sync.Mutex
	// secrets holds a copy-on-write slice, longest value first so that a secret which
	// contains another is masked as a whole.
	secrets atomic.Pointer[[]string]
)

// Register adds a value to the redaction set. Empty and very short values are ignored.
// It is safe for concurrent use and idempotent.
func Register(secret string) {
	if len(secret) < minSecretLen {
		return
	}

	registerMu.Lock()
	defer registerMu.Unlock()

	var next []string
	if cur := secrets.Load(); cur != nil {
		for _, s := range *cur {
			if s == secret {
				return
			}
		}
		next = make([]string, len(*cur), len(*cur)+1)
		copy(next, *cur)
	}
	next = append(next, secret)
	sort.SliceStable(next, func(i, j int) bool { return len(next[i]) > len(next[j]) })
	secrets.Store(&next)
}

// Redact replaces every registered secret in s with Mask, including occurrences embedded
// in a longer string.
func Redact(s string) string {
	cur := secrets.Load()
	if cur == nil || s == "" {
		return s
	}
	for _, secret := range *cur {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, Mask)
		}
	}
	return s
}

// RedactErr returns an error whose message has every registered secret masked. The
// original error stays reachable through errors.Is and errors.As, but its text is never
// rendered. RedactErr(nil) is nil.
func RedactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := Redact(msg)
	if redacted == msg {
		return err
	}
	return &redactedError{msg: redacted, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

// Unwrap exposes the wrapped error for errors.Is and errors.As. Callers must not print
// the result; use Error, which is redacted.
func (e *redactedError) Unwrap() error { return e.err }
