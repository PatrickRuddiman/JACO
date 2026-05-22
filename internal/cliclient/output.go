package cliclient

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

// maxCellWidth is the per-column truncation threshold used by RenderTable.
// Cells longer than this are clipped and terminated with the U+2026 ellipsis
// character so the table stays scannable in a typical 80-col terminal.
const maxCellWidth = 40

// RenderTable writes a columnar table to w. Headers are optional; rows must
// all be the same width. Cells longer than maxCellWidth are truncated with
// `…`. Columns are space-padded for alignment via text/tabwriter.
func RenderTable(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if len(headers) > 0 {
		writeTabRow(tw, headers)
	}
	for _, row := range rows {
		clipped := make([]string, len(row))
		for i, cell := range row {
			clipped[i] = truncate(cell, maxCellWidth)
		}
		writeTabRow(tw, clipped)
	}
	return tw.Flush()
}

// RenderJSON writes v as a pretty-printed JSON document with 2-space indent.
// Map keys are sorted alphabetically by encoding/json; struct fields keep
// their declaration order.
func RenderJSON(w io.Writer, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// RenderJSONStream writes one compact JSON object per line (NDJSON) to w for
// every value received on ch, flushing after each line so streaming consumers
// see events as they arrive. Returns when ch is closed.
func RenderJSONStream(w io.Writer, ch <-chan any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for v := range ch {
		// Encoder.Encode writes a trailing newline — exactly the NDJSON shape.
		if err := enc.Encode(v); err != nil {
			return err
		}
		flushIfPossible(w)
	}
	return nil
}

// RenderYAML writes v as a YAML document via gopkg.in/yaml.v3 with the
// library's default 2-space indent.
func RenderYAML(w io.Writer, v any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(v)
}

func writeTabRow(tw *tabwriter.Writer, cells []string) {
	for i, c := range cells {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, c)
	}
	fmt.Fprintln(tw)
}

// truncate clips s to width-1 runes and appends `…` if s exceeds width.
// Returns s unchanged if it fits.
func truncate(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

func flushIfPossible(w io.Writer) {
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

// SortedMapKeys returns the keys of m in alphabetical order. Useful when
// turning unordered Go maps into deterministic table rows.
func SortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PrintableLines splits s on newline and returns the lines for embedding into
// table cells. Helper used by the existing audit / status commands; lives
// here so the renderer surface stays in one place.
func PrintableLines(s string) []string {
	return strings.Split(s, "\n")
}
