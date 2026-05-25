package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

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

// TestJournalHandler_JSONOneObjectPerLine asserts the daemon's journal handler
// emits exactly one JSON object per line, each carrying PRIORITY (mapped from
// the slog level) and SYSLOG_IDENTIFIER=jacod (issue #38 acceptance).
func TestJournalHandler_JSONOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	h := newJournalHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(h).With(KeySubsystem, "jacod")

	logger.Info("starting", KeyVersion, "1.2.3")
	logger.Error("boom", "error", "kaboom")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d: %q", len(lines), buf.String())
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 is not valid JSON: %v (%q)", err, lines[0])
	}
	if first["msg"] != "starting" {
		t.Errorf("msg = %v, want starting", first["msg"])
	}
	if first["subsystem"] != "jacod" {
		t.Errorf("subsystem = %v, want jacod", first["subsystem"])
	}
	if first["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", first["version"])
	}
	if first["SYSLOG_IDENTIFIER"] != "jacod" {
		t.Errorf("SYSLOG_IDENTIFIER = %v, want jacod", first["SYSLOG_IDENTIFIER"])
	}
	// JSON numbers decode to float64.
	if pr, ok := first["PRIORITY"].(float64); !ok || int(pr) != priInfo {
		t.Errorf("PRIORITY = %v, want %d (info)", first["PRIORITY"], priInfo)
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1 is not valid JSON: %v", err)
	}
	if pr, ok := second["PRIORITY"].(float64); !ok || int(pr) != priErr {
		t.Errorf("error PRIORITY = %v, want %d", second["PRIORITY"], priErr)
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
