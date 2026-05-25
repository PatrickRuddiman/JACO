package logging

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		def  slog.Level
		want slog.Level
	}{
		{"debug", slog.LevelInfo, slog.LevelDebug},
		{"INFO", slog.LevelWarn, slog.LevelInfo},
		{"warn", slog.LevelInfo, slog.LevelWarn},
		{"warning", slog.LevelInfo, slog.LevelWarn},
		{"error", slog.LevelInfo, slog.LevelError},
		{"", slog.LevelWarn, slog.LevelWarn},
		{"nonsense", slog.LevelInfo, slog.LevelInfo},
		// Per-subsystem syntax deferred (Q1): leading token wins, remainder ignored.
		{"debug,scheduler=info", slog.LevelInfo, slog.LevelDebug},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in, c.def); got != c.want {
			t.Errorf("ParseLevel(%q, %v) = %v; want %v", c.in, c.def, got, c.want)
		}
	}
}

func TestPriorityForLevel(t *testing.T) {
	cases := []struct {
		lvl  slog.Level
		want int
	}{
		{slog.LevelDebug, priDebug},
		{slog.LevelInfo, priInfo},
		{slog.LevelWarn, priWarning},
		{slog.LevelError, priErr},
		{slog.LevelError + 4, priErr}, // above ERROR clamps to ERROR
	}
	for _, c := range cases {
		if got := priorityForLevel(c.lvl); got != c.want {
			t.Errorf("priorityForLevel(%v) = %d; want %d", c.lvl, got, c.want)
		}
	}
}

// TestJournalHandler_VarsAreRealJournalFields asserts the native journal
// handler flattens accumulated (.With) and per-record attrs into uppercase
// journal field names plus SYSLOG_IDENTIFIER=jacod — the map that becomes real,
// queryable journal fields via journal.Send (issue #38). This is what makes
// `journalctl -p err` / `journalctl SUBSYSTEM=raft` work, unlike emitting the
// fields inside an opaque JSON MESSAGE.
func TestJournalHandler_VarsAreRealJournalFields(t *testing.T) {
	base := newJournalHandler(slog.LevelDebug).(*journalHandler)
	// Derive like a subsystem logger: .With(subsystem, …) then per-record attrs.
	h := base.WithAttrs([]slog.Attr{
		slog.String(KeySubsystem, "raft"),
		slog.String(KeyNode, "jaco-1"),
	}).(*journalHandler)

	r := slog.NewRecord(time.Time{}, slog.LevelInfo, "leadership change", 0)
	r.AddAttrs(slog.String(KeyRequestID, "abc-123"), slog.Int("term", 7))

	v := h.vars(r)
	want := map[string]string{
		"SYSLOG_IDENTIFIER": "jacod",
		"SUBSYSTEM":         "raft",
		"NODE":              "jaco-1",
		"REQUEST_ID":        "abc-123",
		"TERM":              "7",
	}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("vars[%q] = %q; want %q (full: %v)", k, v[k], w, v)
		}
	}

	// Regression guard: the original bug embedded PRIORITY (and the message) as
	// inert JSON attributes that journald never parsed. With journal.Send,
	// PRIORITY and MESSAGE are protocol arguments, NOT duplicate journal fields.
	for _, banned := range []string{"PRIORITY", "MESSAGE", "priority", "msg"} {
		if _, ok := v[banned]; ok {
			t.Errorf("vars must not carry %q as a field (it is a journal.Send arg): %v", banned, v)
		}
	}

	// PRIORITY maps from the slog level and equals the journal constant.
	if int(journalPriority(slog.LevelError)) != priErr {
		t.Errorf("journalPriority(error) = %d; want %d", journalPriority(slog.LevelError), priErr)
	}
}

// TestNewDaemon_HandlerSelection pins the per-environment handler choice so a
// regression to "JSON-with-inert-PRIORITY to stderr under systemd" is caught:
// under systemd jacod must use the native journal handler (or the JSON fallback
// only when no journal socket is reachable), never a plain stderr handler that
// buries PRIORITY in the message.
func TestNewDaemon_HandlerSelection(t *testing.T) {
	// Off systemd → human-readable text handler.
	t.Setenv("INVOCATION_ID", "")
	if h := NewDaemon(os.Stderr, slog.LevelInfo).Handler(); !isType[*slog.TextHandler](h) {
		t.Errorf("off-systemd handler = %T; want *slog.TextHandler", h)
	}

	// Under systemd → native journalHandler when the journal socket is
	// reachable, else the JSON-to-stderr fallback. Never a TextHandler, and
	// never something that would re-bury PRIORITY as an inert attr.
	t.Setenv("INVOCATION_ID", "test-invocation")
	switch h := NewDaemon(os.Stderr, slog.LevelInfo).Handler(); h.(type) {
	case *journalHandler, *slog.JSONHandler:
		// ok
	default:
		t.Errorf("under-systemd handler = %T; want *journalHandler or *slog.JSONHandler", h)
	}
}

func isType[T any](v any) bool { _, ok := v.(T); return ok }

// TestJournalHandler_GroupAndKeyNormalization checks group folding and key
// normalization into valid journal field names.
func TestJournalHandler_GroupAndKeyNormalization(t *testing.T) {
	g := newJournalHandler(slog.LevelDebug).(*journalHandler).WithGroup("http").(*journalHandler)
	r := slog.NewRecord(time.Time{}, slog.LevelWarn, "x", 0)
	r.AddAttrs(slog.String("status-code", "429"))
	if v := g.vars(r); v["HTTP_STATUS_CODE"] != "429" {
		t.Errorf("group/normalized key missing: %v", v)
	}

	cases := map[string]string{
		"subsystem":   "SUBSYSTEM",
		"request_id":  "REQUEST_ID",
		"status-code": "STATUS_CODE",
		"9lives":      "X9LIVES", // journald fields must start with a letter
		"":            "X",
	}
	for in, want := range cases {
		if got := journalKey(in); got != want {
			t.Errorf("journalKey(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestSubsystemSetsOnceNoDuplicate confirms deriving a subsystem off a bare
// root logger yields exactly one subsystem key (no duplicate).
func TestSubsystemSetsOnceNoDuplicate(t *testing.T) {
	var buf bytes.Buffer
	root := slog.New(slog.NewJSONHandler(&buf, nil))
	Subsystem(root, "scheduler").Info("tick")

	count := strings.Count(buf.String(), `"subsystem":`)
	if count != 1 {
		t.Errorf("subsystem key appears %d times, want 1: %q", count, buf.String())
	}
}

func TestContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := IntoContext(context.Background(), base.With(KeyRequestID, "abc-123"))

	FromContext(ctx, nil).Info("handler line")
	if !strings.Contains(buf.String(), "request_id=abc-123") {
		t.Errorf("context logger lost request_id: %q", buf.String())
	}

	// Missing logger falls back to the provided fallback.
	if FromContext(context.Background(), base) != base {
		t.Errorf("FromContext should return fallback when context has no logger")
	}
	// Nil fallback yields a non-nil discard logger.
	if FromContext(context.Background(), nil) == nil {
		t.Errorf("FromContext returned nil")
	}
}

// TestUnaryInterceptor_AttachesRequestID verifies the interceptor attaches a
// request_id to the context logger and honors an incoming x-request-id.
func TestUnaryInterceptor_AttachesRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	interceptor := UnaryServerInterceptor(base)

	var seen string
	handler := func(ctx context.Context, _ any) (any, error) {
		FromContext(ctx, nil).Info("in handler")
		return nil, nil
	}

	// Incoming x-request-id is honored.
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(requestIDMeta, "caller-supplied-id"))
	_, _ = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/jaco.v1.Deploy/Apply"}, handler)

	out := buf.String()
	if !strings.Contains(out, "request_id=caller-supplied-id") {
		t.Errorf("handler log missing honored request_id: %q", out)
	}
	if !strings.Contains(out, "method=/jaco.v1.Deploy/Apply") {
		t.Errorf("handler log missing method: %q", out)
	}
	_ = seen

	// No incoming id → a fresh UUID is minted (non-empty).
	buf.Reset()
	_, _ = interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/jaco.v1.Cluster/Status"}, handler)
	if !strings.Contains(buf.String(), "request_id=") {
		t.Errorf("expected a minted request_id: %q", buf.String())
	}
	if strings.Contains(buf.String(), "request_id=caller-supplied-id") {
		t.Errorf("stale request_id leaked across RPCs: %q", buf.String())
	}
}
