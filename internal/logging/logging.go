// Package logging is JACO's single structured-logging convention, built on
// the stdlib log/slog. It centralizes root-logger construction for the two
// binaries (jacod, jaco) and the small set of helpers every subsystem uses to
// attach domain context.
//
// Design rules (issue #38):
//   - No third-party logger. log/slog only.
//   - Only the two main() functions build a root logger (via New*). Every
//     other package takes a *slog.Logger via constructor or struct field and
//     derives child loggers with Subsystem / .With. Reaching for
//     slog.Default()/log.Default() in a subsystem is a bug — see the
//     forbid-default lint test in this package.
//   - jacod logs JSON to stderr so the systemd journal can index fields, with
//     SYSLOG_IDENTIFIER=jacod and a journal PRIORITY derived from the slog
//     level. When not running under systemd (no INVOCATION_ID) it falls back
//     to a human-readable text handler.
//   - jaco (the CLI) logs human-readable text to stderr, default WARN.
//
// Sensitive-data hygiene: callers must never pass bearer tokens, private
// keys, or audit-event payloads as log attributes. This package does not (and
// cannot) scrub them; it only standardizes the plumbing.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Attribute keys used across subsystems. Centralized so every subsystem
// spells them the same way and operators can rely on stable journal fields.
const (
	KeySubsystem  = "subsystem"
	KeyNode       = "node"
	KeyDeployment = "deployment"
	KeyReplicaID  = "replica_id"
	KeyRequestID  = "request_id"
	KeyMethod     = "method"
	KeyPeer       = "peer"
	KeyVersion    = "version"
	KeyReason     = "reason"
)

// ParseLevel maps a JACO_LOG / --log-level string onto a slog.Level. Unknown
// or empty values return def. Recognizes debug|info|warn|warning|error
// (case-insensitive).
//
// The per-subsystem override syntax (JACO_LOG=info,scheduler=debug) is
// deferred for v1 (issue #38 Q1): any comma-separated remainder is ignored
// and only the leading global token is honored. This is the documented
// extension point — a future version parses the remainder into per-subsystem
// levels.
func ParseLevel(s string, def slog.Level) slog.Level {
	if s == "" {
		return def
	}
	// v1: honor only the leading global token, ignore per-subsystem remainder.
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return def
	}
}

// LevelFromEnv resolves the effective level from the JACO_LOG env var, falling
// back to def when unset/unrecognized.
func LevelFromEnv(def slog.Level) slog.Level {
	return ParseLevel(os.Getenv("JACO_LOG"), def)
}

// underSystemd reports whether the process was started by systemd, detected
// via the INVOCATION_ID env var systemd sets for every unit (issue #38 Q5).
func underSystemd() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

// NewDaemon builds jacod's ROOT logger. Under systemd it emits one JSON
// object per line to out (normally os.Stderr) so journald indexes fields;
// each record carries SYSLOG_IDENTIFIER=jacod and a journal PRIORITY derived
// from the slog level (debug=7, info=6, warn=4, error=3). When not under
// systemd it falls back to a human-readable text handler for local runs.
//
// The returned logger carries NO subsystem attribute — cmd/jacod derives its
// own (Subsystem(root, "jacod")) for main-level lifecycle logs, and passes
// this bare root to the gRPC server so each subsystem sets its own subsystem
// exactly once via Subsystem (avoiding duplicate subsystem keys).
func NewDaemon(out *os.File, level slog.Level) *slog.Logger {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if underSystemd() {
		h = newJournalHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	return slog.New(h)
}

// NewCLI builds jaco's ROOT logger: a human-readable text handler to out
// (os.Stderr), default level WARN so operator output stays uncluttered.
// JACO_LOG / --log-level raise it. Like NewDaemon it carries no subsystem
// attribute; callers derive one via Subsystem.
func NewCLI(out *os.File, level slog.Level) *slog.Logger {
	h := slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// Subsystem derives a child logger tagged subsystem=name. Every subsystem
// constructor should call this (or accept an already-tagged logger) so log
// lines are filterable by subsystem in the journal.
//
// IMPORTANT: pass a ROOT logger (one with no subsystem attr yet) so the
// subsystem key is set exactly once. Passing an already-subsystem-tagged
// logger produces duplicate subsystem keys in the JSON output.
//
// A nil base returns a logger backed by a discard handler — subsystems that
// were handed no logger stay silent rather than panicking or leaking to
// slog.Default().
func Subsystem(base *slog.Logger, name string) *slog.Logger {
	if base == nil {
		base = Discard()
	}
	return base.With(KeySubsystem, name)
}

// Discard returns a logger that drops every record. Used as the nil-safe
// fallback so subsystems never reach for slog.Default().
func Discard() *slog.Logger {
	return slog.New(discardHandler{})
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
