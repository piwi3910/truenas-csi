package obs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// testAPIKey is shaped like a TrueNAS API key (id, dash, 64 characters) but is
// obviously synthetic. A real credential must never become a test fixture: it
// ends up in git history, where it lives forever.
const testAPIKey = "9-TestFixtureNotARealApiKey000000000000000000000000000000000000000"

const testCHAP = "Ch4pS3cr3t" + "V4lu3"

// TestNoSecretsInOutput catches a redaction path that lets a registered credential reach
// a log line or an error message.
func TestNoSecretsInOutput(t *testing.T) {
	Register(testAPIKey)
	Register(testCHAP)

	var buf bytes.Buffer
	SetLogOutput(&buf, slog.LevelDebug)
	t.Cleanup(func() { SetLogOutput(nil, slog.LevelInfo) })

	ctx := WithVolume(context.Background(), "nas1/nfs/Pool0/k8s/pvc-1")
	lg := Logger(ctx)
	lg.Info("calling middleware with api_key=" + testAPIKey)
	lg.Error("chap login failed", "secret", testCHAP)
	lg.With("api_key", testAPIKey).Info("attached credential")

	out := buf.String()
	if strings.Contains(out, testAPIKey) {
		t.Errorf("api key leaked into log output: %q", out)
	}
	if strings.Contains(out, testCHAP) {
		t.Errorf("chap secret leaked into log output: %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("expected redaction marker in log output: %q", out)
	}

	err := RedactErr(errors.New("rpc error: authentication failed for key " + testAPIKey))
	if err == nil {
		t.Fatal("RedactErr(non-nil) returned nil")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("api key leaked into error text: %q", err.Error())
	}
	if RedactErr(nil) != nil {
		t.Error("RedactErr(nil) must be nil")
	}
}

// TestRedactHandlesSubstrings catches both an anchored-only matcher and a registration
// guard that lets a trivially short value mask ordinary text.
func TestRedactHandlesSubstrings(t *testing.T) {
	const embedded = "supersecretvalue123"
	Register(embedded)

	got := Redact("prefix " + embedded + " suffix")
	if strings.Contains(got, embedded) {
		t.Errorf("embedded secret not masked: %q", got)
	}
	if got != "prefix [redacted] suffix" {
		t.Errorf("unexpected redaction result: %q", got)
	}

	Register("")
	Register("a")
	Register("ab")

	const sentence = "a normal sentence about a dataset ab and a pool"
	if out := Redact(sentence); out != sentence {
		t.Errorf("short registration masked ordinary text: %q", out)
	}
}
