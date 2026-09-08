package truenas

import (
	"errors"
	"fmt"
	"strings"
)

// ErrAuthFailed means the appliance rejected our credentials.
//
// This is TERMINAL and must never be retried. TrueNAS revokes an API key after
// failed authentication over an insecure transport, and a reconnect loop that
// re-authenticates on every error can destroy the driver's own credential and
// break every volume operation in the cluster.
var ErrAuthFailed = errors.New("truenas authentication rejected")

// ErrConnClosed means the connection dropped mid-call; the call may be retried.
var ErrConnClosed = errors.New("truenas connection closed")

// ErrTooManyConcurrent is JSON-RPC -32000: the appliance's in-flight ceiling.
// It is backpressure, not a volume failure, and should be retried with backoff.
var ErrTooManyConcurrent = errors.New("truenas rejected the call: too many concurrent calls")

// CallError is a JSON-RPC error returned by a method call.
//
// Note that ErrName is NOT reliable: the appliance reports EINVAL for conditions
// whose true errno (visible only as a "[ENOENT] ..." prefix inside Reason) is
// something else entirely. Establish state with an explicit query rather than
// inferring it from these fields.
type CallError struct {
	Method  string
	Code    int
	ErrName string
	Reason  string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("%s: jsonrpc %d %s: %s", e.Method, e.Code, e.ErrName, e.Reason)
}

// IsNotFound reports whether the error looks like a missing object. It reads the
// "[ERRNO]" prefix the appliance embeds in Reason, because ErrName lies.
func IsNotFound(err error) bool {
	var ce *CallError
	if !errors.As(err, &ce) {
		return false
	}
	return len(ce.Reason) >= 8 && ce.Reason[:8] == "[ENOENT]"
}

// IsBusy reports whether the appliance refused an operation because ZFS
// considers the dataset busy: a mount or a zvol device the kernel has not
// released yet, or a snapshot that still has dependent clones.
//
// It reads Reason as well as ErrName for the same reason IsNotFound does — the
// middleware reports EINVAL for conditions whose real errno appears only inside
// the text — and it lives here rather than in each backend because "busy"
// decides whether a destructive operation is retried or abandoned, and that
// decision must be identical everywhere it is taken.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	var ce *CallError
	if errors.As(err, &ce) {
		if ce.ErrName == "EBUSY" || strings.Contains(ce.Reason, "dataset is busy") {
			return true
		}
	}
	return strings.Contains(err.Error(), "EBUSY") ||
		strings.Contains(err.Error(), "dataset is busy")
}
