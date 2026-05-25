package logging

import (
	"context"
	"io"
	"log/slog"
)

// syslog severity codes, per RFC 5424 / sd-daemon(3). The journal indexes the
// PRIORITY field with these numeric values.
const (
	priErr     = 3 // LOG_ERR
	priWarning = 4 // LOG_WARNING
	priInfo    = 6 // LOG_INFO
	priDebug   = 7 // LOG_DEBUG
)

// priorityForLevel maps an slog level onto a journal PRIORITY (issue #38):
// debug=7, info=6, warn=4, error=3. Levels above ERROR clamp to ERROR; the
// gap between WARN and INFO maps to INFO.
func priorityForLevel(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return priErr
	case l >= slog.LevelWarn:
		return priWarning
	case l >= slog.LevelInfo:
		return priInfo
	default:
		return priDebug
	}
}

// journalHandler wraps a slog.JSONHandler and injects two journal-oriented
// fields on every record: SYSLOG_IDENTIFIER=jacod (so the unit's logs are
// filterable with `journalctl -t jacod` / SYSLOG_IDENTIFIER=jacod) and
// PRIORITY derived from the record's level. journald, reading one JSON object
// per line on stderr, surfaces both as native journal fields.
//
// It implements slog.Handler directly (rather than embedding) so WithAttrs /
// WithGroup return a journalHandler wrapping the inner handler's result,
// preserving the PRIORITY/identifier injection through .With chains.
type journalHandler struct {
	inner slog.Handler
}

func newJournalHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	return &journalHandler{inner: slog.NewJSONHandler(w, opts)}
}

func (h *journalHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *journalHandler) Handle(ctx context.Context, r slog.Record) error {
	// Clone so we don't mutate the caller's record, then prepend the journal
	// fields. PRIORITY is an int the journal reads numerically.
	rec := r.Clone()
	rec.AddAttrs(
		slog.Int("PRIORITY", priorityForLevel(r.Level)),
		slog.String("SYSLOG_IDENTIFIER", "jacod"),
	)
	return h.inner.Handle(ctx, rec)
}

func (h *journalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &journalHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *journalHandler) WithGroup(name string) slog.Handler {
	return &journalHandler{inner: h.inner.WithGroup(name)}
}
