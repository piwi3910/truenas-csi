package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// VolumeIDKey is the log attribute carrying the CSI volume handle.
const VolumeIDKey = "volume_id"

type ctxKey struct{}

var volumeCtxKey ctxKey

// base is the handler every Logger wraps. It is replaced wholesale by SetLogOutput.
var base atomic.Pointer[slog.Handler]

func init() {
	SetLogOutput(nil, slog.LevelInfo)
}

// SetLogOutput redirects log output to w at the given level. A nil writer restores
// os.Stderr. Tests use it to capture output.
func SetLogOutput(w io.Writer, level slog.Level) {
	if w == nil {
		w = os.Stderr
	}
	var h slog.Handler = slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
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
