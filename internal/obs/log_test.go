package obs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    slog.Level
		wantOK  bool
		wantErr bool
	}{
		{name: "debug", want: slog.LevelDebug, wantOK: true},
		{name: "info", want: slog.LevelInfo, wantOK: true},
		{name: "warn", want: slog.LevelWarn, wantOK: true},
		{name: "error", want: slog.LevelError, wantOK: true},
		{name: "", want: slog.LevelInfo, wantOK: true},
		{name: "verbose", want: slog.LevelInfo, wantOK: false},
	} {
		t.Run("level "+tc.name, func(t *testing.T) {
			got, ok := ParseLevel(tc.name)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ParseLevel(%q) = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSetLogLevelAffectsLoggersAlreadyHandedOut is the behaviour the dynamic
// log level depends on: an operator raising verbosity during an incident must
// affect every logger in the process, including ones built before the change.
// A level captured into a handler at construction would silently ignore them.
func TestSetLogLevelAffectsLoggersAlreadyHandedOut(t *testing.T) {
	var buf bytes.Buffer
	restore(t)
	SetLogOutput(&buf, slog.LevelInfo)

	lg := Logger(context.Background()) // obtained BEFORE the level changes
	lg.Debug("quiet")
	if buf.Len() != 0 {
		t.Fatalf("a debug record was emitted at info level: %s", buf.String())
	}

	SetLogLevel(slog.LevelDebug)
	lg.Debug("loud")
	if !strings.Contains(buf.String(), "loud") {
		t.Fatalf("raising the level did not reach an existing logger: %q", buf.String())
	}
	if LogLevel() != slog.LevelDebug {
		t.Fatalf("LogLevel = %v", LogLevel())
	}
}

func TestSetLogFormat(t *testing.T) {
	var buf bytes.Buffer
	restore(t)
	SetLogOutput(&buf, slog.LevelInfo)

	if err := SetLogFormat("logfmt"); err == nil {
		t.Fatal("an unknown format must be refused")
	}
	if LogFormat() != FormatText {
		t.Fatalf("a refused format changed the setting to %q", LogFormat())
	}

	if err := SetLogFormat(FormatJSON); err != nil {
		t.Fatal(err)
	}
	Logger(context.Background()).Info("hello", "backend", "nas1")
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") || !strings.Contains(out, `"backend":"nas1"`) {
		t.Fatalf("output is not JSON: %q", out)
	}
}

// TestJSONHandlerStillRedacts: switching format must not open a route around
// the redacting handler. A credential in a log line is the one bug this package
// exists to prevent.
func TestJSONHandlerStillRedacts(t *testing.T) {
	var buf bytes.Buffer
	restore(t)
	SetLogOutput(&buf, slog.LevelInfo)
	if err := SetLogFormat(FormatJSON); err != nil {
		t.Fatal(err)
	}
	Register("8-supersecret")
	Logger(context.Background()).Info("dialling", "key", "8-supersecret")
	if strings.Contains(buf.String(), "8-supersecret") {
		t.Fatalf("the JSON handler leaked a credential: %s", buf.String())
	}
}

// TestLogSettingsAreConcurrencySafe: the ConfigMap watcher changes these while
// the driver logs. Run with -race.
func TestLogSettingsAreConcurrencySafe(t *testing.T) {
	restore(t)
	SetLogOutput(&syncBuf{}, slog.LevelInfo)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetLogLevel(slog.LevelDebug)
				_ = SetLogFormat(FormatJSON)
				SetLogLevel(slog.LevelInfo)
				_ = SetLogFormat(FormatText)
				Logger(context.Background()).Info("churn")
			}
		}(i)
	}
	wg.Wait()
}

// restore puts the package back to its default logging state after a test, so
// tests that capture output do not leak settings into each other.
func restore(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = SetLogFormat(FormatText)
		SetLogOutput(nil, slog.LevelInfo)
	})
}

// syncBuf is a writer that tolerates concurrent writes.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
