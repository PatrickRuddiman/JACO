package logging

import (
	"context"
	"log/slog"
	"strings"

	"github.com/coreos/go-systemd/v22/journal"
)

// syslog severity codes, per RFC 5424 / sd-daemon(3). The journal indexes the
// PRIORITY field with these numeric values. They match journal.Pri* exactly.
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

// journalPriority is priorityForLevel typed for journal.Send. The numeric
// values are identical (journal.PriErr == 3, etc.).
func journalPriority(l slog.Level) journal.Priority {
	return journal.Priority(priorityForLevel(l))
}

// journalHandler writes each record to the systemd journal's native socket via
// journal.Send. Unlike emitting JSON to stderr (where journald treats the line
// as an opaque MESSAGE), the native protocol makes PRIORITY, SYSLOG_IDENTIFIER,
// and every slog attr REAL journal fields — so `journalctl -p err`,
// `journalctl SUBSYSTEM=raft`, etc. filter correctly.
//
// slog attrs are flattened to uppercase journal field names (subsystem ->
// SUBSYSTEM, request_id -> REQUEST_ID); groups join with '_'. The message is
// the journal MESSAGE; PRIORITY derives from the record's level.
type journalHandler struct {
	level  slog.Leveler
	pre    []field // accumulated via WithAttrs, already group-prefixed + stringified
	prefix string  // current group prefix, e.g. "HTTP_"
}

type field struct{ key, val string }

func newJournalHandler(level slog.Leveler) slog.Handler {
	return &journalHandler{level: level}
}

func (h *journalHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *journalHandler) Handle(_ context.Context, r slog.Record) error {
	return journal.Send(r.Message, journalPriority(r.Level), h.vars(r))
}

// vars builds the journal field map for a record: SYSLOG_IDENTIFIER plus every
// accumulated and per-record attr, flattened to journal field names. Split out
// from Handle so it is unit-testable without a journal socket.
func (h *journalHandler) vars(r slog.Record) map[string]string {
	vars := make(map[string]string, len(h.pre)+r.NumAttrs()+1)
	vars["SYSLOG_IDENTIFIER"] = "jacod"
	for _, f := range h.pre {
		vars[f.key] = f.val
	}
	r.Attrs(func(a slog.Attr) bool {
		for _, f := range flattenAttr(h.prefix, a) {
			vars[f.key] = f.val
		}
		return true
	})
	return vars
}

func (h *journalHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	nh := *h
	nh.pre = append(append([]field{}, h.pre...), flattenAttrs(h.prefix, as)...)
	return &nh
}

func (h *journalHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.prefix = h.prefix + journalKey(name) + "_"
	return &nh
}

func flattenAttrs(prefix string, as []slog.Attr) []field {
	var out []field
	for _, a := range as {
		out = append(out, flattenAttr(prefix, a)...)
	}
	return out
}

// flattenAttr resolves an attr and emits one field per scalar value; group
// attrs recurse with their key folded into the prefix. Empty attrs are dropped.
func flattenAttr(prefix string, a slog.Attr) []field {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return nil
	}
	if a.Value.Kind() == slog.KindGroup {
		gp := prefix
		if a.Key != "" {
			gp = prefix + journalKey(a.Key) + "_"
		}
		var out []field
		for _, ga := range a.Value.Group() {
			out = append(out, flattenAttr(gp, ga)...)
		}
		return out
	}
	return []field{{key: prefix + journalKey(a.Key), val: a.Value.String()}}
}

// journalKey normalizes an slog attr key into a valid journal field name:
// uppercase, with any character outside [A-Z0-9_] replaced by '_'. journald
// requires field names to start with a letter (leading '_' is reserved for its
// own trusted fields), so a name that would start with a digit/underscore is
// prefixed with 'X'. Our standard keys (subsystem, request_id, …) are already
// letter-leading, so this only guards exotic caller keys.
func journalKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range k {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "X"
	}
	if s[0] < 'A' || s[0] > 'Z' {
		return "X" + s
	}
	return s
}
