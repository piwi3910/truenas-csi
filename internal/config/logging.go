package config

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Logging is the observability configuration that can be changed while the
// driver runs.
//
// It lives in a ConfigMap, not in the credential Secret, precisely so it can be
// edited by whoever is handling an incident without giving them access to an
// API key — raising verbosity at 03:00 should not require a rollout or a
// credential.
//
// The scope is deliberately closed at level and format. Anything else an
// operator might want to change live is either identity (see ErrRestartRequired)
// or a listener address that is already in a pod spec.
type Logging struct {
	// Level is one of debug, info, warn, error. Empty means info.
	Level string `yaml:"logLevel"`
	// Format is text or json. Empty means text.
	Format string `yaml:"logFormat"`
}

// Log formats this driver can emit.
const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

// LogLevels are the accepted level names, in increasing severity.
var LogLevels = []string{"debug", "info", "warn", "error"}

// LogFormats are the accepted format names.
var LogFormats = []string{LogFormatText, LogFormatJSON}

// Normalised returns the logging settings with defaults filled in and case and
// whitespace removed.
func (l Logging) Normalised() Logging {
	out := Logging{
		Level:  strings.ToLower(strings.TrimSpace(l.Level)),
		Format: strings.ToLower(strings.TrimSpace(l.Format)),
	}
	if out.Level == "" {
		out.Level = "info"
	}
	if out.Format == "" {
		out.Format = LogFormatText
	}
	return out
}

// Validate rejects a level or format this driver cannot emit.
//
// An unknown value is an error rather than a silent fall back to info: the
// whole point of the live ConfigMap is that somebody typed "debug" during an
// incident, and quietly staying at info would waste the outage.
func (l Logging) Validate() error {
	n := l.Normalised()
	if !contains(LogLevels, n.Level) {
		return fmt.Errorf("logLevel %q is not one of %s", l.Level, strings.Join(LogLevels, ", "))
	}
	if !contains(LogFormats, n.Format) {
		return fmt.Errorf("logFormat %q is not one of %s", l.Format, strings.Join(LogFormats, ", "))
	}
	return nil
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// LoadLogging reads and validates a mounted logging ConfigMap.
func LoadLogging(path string) (Logging, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Logging{}, fmt.Errorf("read logging config: %w", err)
	}
	var l Logging
	if err := yaml.Unmarshal(raw, &l); err != nil {
		return Logging{}, fmt.Errorf("parse logging config: %w", err)
	}
	if err := l.Validate(); err != nil {
		return Logging{}, err
	}
	return l.Normalised(), nil
}

// WatchLogging calls apply with the new settings every time the mounted file
// changes to something valid and different, until ctx ends.
//
// An invalid edit is reported through onError and the driver keeps logging
// exactly as it was. Verbosity is not worth a crash loop.
func WatchLogging(ctx context.Context, path string, cur Logging, apply func(Logging), onError func(error)) error {
	cur = cur.Normalised()
	return WatchFile(ctx, path, func() {
		next, err := LoadLogging(path)
		if err != nil {
			if onError != nil {
				onError(fmt.Errorf("reload %s: %w", path, err))
			}
			return
		}
		if next == cur {
			return
		}
		cur = next
		if apply != nil {
			apply(next)
		}
	})
}
