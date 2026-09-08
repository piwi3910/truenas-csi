package config

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLoggingValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         Logging
		wantErrIn  string
		wantLevel  string
		wantFormat string
	}{
		{name: "defaults", in: Logging{}, wantLevel: "info", wantFormat: LogFormatText},
		{name: "debug json", in: Logging{Level: "debug", Format: "json"},
			wantLevel: "debug", wantFormat: LogFormatJSON},
		{name: "case and space", in: Logging{Level: "  WARN ", Format: "TEXT"},
			wantLevel: "warn", wantFormat: LogFormatText},
		{name: "unknown level", in: Logging{Level: "verbose"}, wantErrIn: "logLevel"},
		{name: "unknown format", in: Logging{Format: "logfmt"}, wantErrIn: "logFormat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Fatalf("want an error naming %s, got %v", tc.wantErrIn, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := tc.in.Normalised()
			if got.Level != tc.wantLevel || got.Format != tc.wantFormat {
				t.Fatalf("Normalised = %+v, want level %q format %q", got, tc.wantLevel, tc.wantFormat)
			}
		})
	}
}

// TestWatchLoggingAppliesAndRejects: an operator raising verbosity mid-incident
// must take effect, and a typo must leave the driver logging exactly as it was
// rather than crash-looping it over a log level.
func TestWatchLoggingAppliesAndRejects(t *testing.T) {
	dir := t.TempDir()
	path := projectSecret(t, dir, "logging.yaml", "logLevel: info\nlogFormat: text\n", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applied := make(chan Logging, 4)
	failed := make(chan error, 4)
	go func() {
		_ = WatchLogging(ctx, path, Logging{Level: "info"},
			func(l Logging) { applied <- l },
			func(err error) { failed <- err })
	}()
	time.Sleep(200 * time.Millisecond)

	projectSecret(t, dir, "logging.yaml", "logLevel: debug\nlogFormat: json\n", 2)
	select {
	case got := <-applied:
		if got.Level != "debug" || got.Format != LogFormatJSON {
			t.Fatalf("applied %+v", got)
		}
	case err := <-failed:
		t.Fatalf("a valid change was rejected: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("a raised log level was never applied")
	}

	projectSecret(t, dir, "logging.yaml", "logLevel: verbose\n", 3)
	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "logLevel") {
			t.Fatalf("unexpected error: %v", err)
		}
	case got := <-applied:
		t.Fatalf("an invalid log level was applied: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("an invalid log level was never reported")
	}
}

func TestLoadLoggingRejectsAMissingFile(t *testing.T) {
	if _, err := LoadLogging(t.TempDir() + "/absent.yaml"); err == nil {
		t.Fatal("a missing logging config must be reported, not silently defaulted")
	}
}
