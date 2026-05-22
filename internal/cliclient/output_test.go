package cliclient_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestRenderTable_PrintsHeadersAndRows(t *testing.T) {
	var buf bytes.Buffer
	if err := cliclient.RenderTable(&buf, []string{"NAME", "STATUS"},
		[][]string{{"node-a", "READY"}, {"node-b", "JOINING"}}); err != nil {
		t.Fatalf("RenderTable: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"NAME", "STATUS", "node-a", "READY", "node-b", "JOINING"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTable_TruncatesLongCells(t *testing.T) {
	long := strings.Repeat("x", 60)
	var buf bytes.Buffer
	if err := cliclient.RenderTable(&buf, []string{"COL"}, [][]string{{long}}); err != nil {
		t.Fatalf("RenderTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation ellipsis in output:\n%s", out)
	}
	// The full 60-char string must NOT appear.
	if strings.Contains(out, long) {
		t.Errorf("untruncated string leaked into output")
	}
}

func TestRenderTable_NoHeadersStillRenders(t *testing.T) {
	var buf bytes.Buffer
	if err := cliclient.RenderTable(&buf, nil, [][]string{{"only", "row"}}); err != nil {
		t.Fatalf("RenderTable: %v", err)
	}
	if !strings.Contains(buf.String(), "only") {
		t.Errorf("missing row content")
	}
}

func TestRenderJSON_MapKeysAlphabetical(t *testing.T) {
	in := map[string]any{
		"zeta":    1,
		"alpha":   2,
		"middle":  3,
		"another": 4,
	}
	var buf bytes.Buffer
	if err := cliclient.RenderJSON(&buf, in); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	// Parse the output and verify keys came out sorted (encoding/json sorts
	// map keys alphabetically before emitting).
	keys := orderedKeys(t, buf.Bytes())
	want := []string{"alpha", "another", "middle", "zeta"}
	if !equalStringSlices(keys, want) {
		t.Errorf("ordered keys = %v, want %v", keys, want)
	}
}

func TestRenderJSON_PrettyPrintedTwoSpaceIndent(t *testing.T) {
	var buf bytes.Buffer
	cliclient.RenderJSON(&buf, map[string]any{"name": "test"})
	out := buf.String()
	if !strings.Contains(out, "  \"name\": \"test\"") {
		t.Errorf("expected 2-space indent in pretty output:\n%s", out)
	}
}

func TestRenderYAML_RoundTrips(t *testing.T) {
	in := map[string]any{
		"a": 1,
		"b": []int{2, 3, 4},
		"c": map[string]any{"nested": true},
	}
	var buf bytes.Buffer
	if err := cliclient.RenderYAML(&buf, in); err != nil {
		t.Fatalf("RenderYAML: %v", err)
	}
	// Round-trip parse to verify the output is valid YAML and the shape matches.
	var got map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("YAML parse: %v\n%s", err, buf.String())
	}
	if got["a"].(int) != 1 {
		t.Errorf("a = %v, want 1", got["a"])
	}
	if _, ok := got["c"].(map[string]any); !ok {
		t.Errorf("c missing or wrong type: %+v", got["c"])
	}
}

func TestRenderJSONStream_EmitsNDJSON(t *testing.T) {
	ch := make(chan any, 3)
	ch <- map[string]any{"i": 1}
	ch <- map[string]any{"i": 2}
	ch <- map[string]any{"i": 3}
	close(ch)

	var buf bytes.Buffer
	if err := cliclient.RenderJSONStream(&buf, ch); err != nil {
		t.Fatalf("RenderJSONStream: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ndjson lines = %d, want 3:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d parse: %v\n%q", i, err, line)
		}
		// json.Unmarshal returns float64 for numeric literals.
		if got["i"].(float64) != float64(i+1) {
			t.Errorf("line %d: i = %v, want %d", i, got["i"], i+1)
		}
	}
}

func TestRenderError_StableDetailOrder(t *testing.T) {
	var buf bytes.Buffer
	cliclient.RenderError(&buf, &pb.Error{
		Code:    "validation_failed",
		Message: "service `web` has no replicas",
		Details: map[string]string{
			"zeta":    "1",
			"alpha":   "2",
			"middle":  "3",
			"another": "4",
		},
	})
	out := buf.String()
	if !strings.HasPrefix(out, "Error: validation_failed — service `web` has no replicas\n") {
		t.Errorf("missing header line:\n%s", out)
	}
	// Detail lines must be alphabetical: alpha, another, middle, zeta.
	idxAlpha := strings.Index(out, "alpha=2")
	idxAnother := strings.Index(out, "another=4")
	idxMiddle := strings.Index(out, "middle=3")
	idxZeta := strings.Index(out, "zeta=1")
	for _, p := range []int{idxAlpha, idxAnother, idxMiddle, idxZeta} {
		if p < 0 {
			t.Fatalf("missing detail line in output:\n%s", out)
		}
	}
	if !(idxAlpha < idxAnother && idxAnother < idxMiddle && idxMiddle < idxZeta) {
		t.Errorf("detail keys not alphabetical in output:\n%s", out)
	}
}

func TestRenderError_NilHandled(t *testing.T) {
	var buf bytes.Buffer
	cliclient.RenderError(&buf, nil)
	if !strings.Contains(buf.String(), "unknown") {
		t.Errorf("nil error should render a placeholder:\n%s", buf.String())
	}
}

func TestRenderConnectionError_Format(t *testing.T) {
	var buf bytes.Buffer
	cliclient.RenderConnectionError(&buf, "host:7000", "TLS verify failed")
	want := "Connection error: host:7000: TLS verify failed\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestExtractError_GrpcStatusWithoutDetailsSynthesizes(t *testing.T) {
	// Status error without proto details — ExtractError synthesizes a
	// pb.Error from the status code + message.
	err := grpcStatusErr("garbage_token")
	e := cliclient.ExtractError(err)
	if e == nil {
		t.Fatalf("ExtractError returned nil")
	}
	if !strings.Contains(e.GetMessage(), "garbage_token") {
		t.Errorf("synthesized message = %q", e.GetMessage())
	}
}

func TestSortedMapKeys(t *testing.T) {
	keys := cliclient.SortedMapKeys(map[string]int{"c": 1, "a": 2, "b": 3})
	want := []string{"a", "b", "c"}
	if !equalStringSlices(keys, want) {
		t.Errorf("SortedMapKeys = %v, want %v", keys, want)
	}
}

// --- helpers -----------------------------------------------------------------

// orderedKeys parses jsonBody and returns top-level keys in the order they
// appear (encoding/json sorts them alphabetically for maps).
func orderedKeys(t *testing.T, jsonBody []byte) []string {
	t.Helper()
	// Use json.Decoder.Token to walk the object in source order.
	dec := json.NewDecoder(strings.NewReader(string(jsonBody)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("expected '{', got %v", tok)
	}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		out = append(out, tok.(string))
		// Skip the value.
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("Decode value: %v", err)
		}
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// grpcStatusErr is implemented in helpers_grpc_test.go.
