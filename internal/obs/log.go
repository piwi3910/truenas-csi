package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// VolumeIDKey is the log attribute carrying the CSI volume handle.
const VolumeIDKey = "volume_id"

type ctxKey struct{}

var volumeCtxKey ctxKey

// base is the handler every Logger wraps. It is replaced wholesale by
// SetLogOutput and SetLogFormat.
var base atomic.Pointer[slog.Handler]

// The live logging state.
//
// level is a slog.LevelVar rather than a plain Level because it is the one
// setting that has to change UNDER handlers that already exist: a logger
// obtained before an operator raised verbosity must start emitting debug
// records too, and every Logger call would otherwise have to re-read a global.
// Format cannot work that way — a text and a JSON handler are different types —
// so changing it rebuilds the handler, which is fine because nobody changes
// format during an incident.
var (
	level = new(slog.LevelVar)

	outMu     sync.Mutex
	logWriter io.Writer = os.Stderr
	logFormat           = FormatText
)

// Log formats this driver can emit. They mirror config.LogFormat*; obs does not
// import config, so that a logging package stays usable from anywhere.
const (
	FormatText = "text"
	FormatJSON = "json"
)

func init() {
	SetLogOutput(nil, slog.LevelInfo)
}

// SetLogOutput redirects log output to w at the given level, in the current
// format. A nil writer restores os.Stderr. Tests use it to capture output.
func SetLogOutput(w io.Writer, l slog.Level) {
	outMu.Lock()
	defer outMu.Unlock()
	if w == nil {
		w = os.Stderr
	}
	logWriter = w
	level.Set(l)
	rebuildLocked()
}

// SetLogLevel changes the verbosity of every logger, including ones already
// handed out. It is the hot path of the watched logging ConfigMap.
func SetLogLevel(l slog.Level) { level.Set(l) }

// LogLevel is the level currently in force.
func LogLevel() slog.Level { return level.Level() }

// SetLogFormat switches between the text and JSON handlers. An unknown format
// is an error and leaves the current handler alone.
func SetLogFormat(format string) error {
	if format != FormatText && format != FormatJSON {
		return fmt.Errorf("log format %q is not %q or %q", format, FormatText, FormatJSON)
	}
	outMu.Lock()
	defer outMu.Unlock()
	if logFormat == format {
		return nil
	}
	logFormat = format
	rebuildLocked()
	return nil
}

// LogFormat is the format currently in force.
func LogFormat() string {
	outMu.Lock()
	defer outMu.Unlock()
	return logFormat
}

// ParseLevel maps a configured level name onto slog. An unrecognised name is
// reported rather than silently treated as info, so a typo in a ConfigMap does
// not quietly cost an operator their debug logs.
func ParseLevel(name string) (slog.Level, bool) {
	switch name {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// rebuildLocked replaces the base handler. The caller holds outMu.
func rebuildLocked() {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if logFormat == FormatJSON {
		h = slog.NewJSONHandler(logWriter, opts)
	} else {
		h = slog.NewTextHandler(logWriter, opts)
	}
	base.Store(&h)
}

// WithVolume returns a context carrying the volume handle, so that every log record
// emitted under it is attributed to that volume.
func WithVolume(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, volumeCtxKey, id)
}

// VolumeFromContext returns the volume handle carried by ctx, if any.
func VolumeFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(volumeCtxKey).(string)
	return id, ok && id != ""
}

// Logger returns a logger whose output is redacted and which carries the context's
// volume id, when present, as a volume_id attribute.
func Logger(ctx context.Context) *slog.Logger {
	h := redactHandler{inner: *base.Load()}
	lg := slog.New(h)
	if id, ok := VolumeFromContext(ctx); ok {
		lg = lg.With(slog.String(VolumeIDKey, id))
	}
	return lg
}

// redactHandler masks every registered secret in the message and in attribute values
// before they reach the underlying handler. It is the only path Logger hands out, so a
// credential cannot reach the log by any route, including With and WithGroup.
type redactHandler struct {
	inner slog.Handler
}

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		safe[i] = redactAttr(a)
	}
	return redactHandler{inner: h.inner.WithAttrs(safe)}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		g := v.Group()
		safe := make([]any, 0, len(g))
		for _, sub := range g {
			safe = append(safe, redactAttr(sub))
		}
		return slog.Group(a.Key, safe...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		return slog.String(a.Key, Redact(v.String()))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
