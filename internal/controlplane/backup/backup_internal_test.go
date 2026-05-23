package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// TestMajor_StripsAfterFirstDot — drives both branches.
func TestMajor_StripsAfterFirstDot(t *testing.T) {
	if got := major("1.2.3"); got != "1" {
		t.Errorf("major(1.2.3) = %q, want 1", got)
	}
	if got := major("0.0.1-dev"); got != "0" {
		t.Errorf("major(0.0.1-dev) = %q, want 0", got)
	}
	if got := major(""); got != "" {
		t.Errorf("major(empty) = %q, want empty", got)
	}
	if got := major("plain"); got != "plain" {
		t.Errorf("major(no-dot) = %q, want plain", got)
	}
}

// TestMajorVersionsCompatible_EmptyTreatedAsCompatible — older backups
// (pre-ldflags) and dev builds shouldn't be rejected just because one
// of the two strings is empty.
func TestMajorVersionsCompatible_EmptyTreatedAsCompatible(t *testing.T) {
	if !majorVersionsCompatible("", "1.0.0") {
		t.Errorf("empty/1.0.0 should be compatible")
	}
	if !majorVersionsCompatible("1.0.0", "") {
		t.Errorf("1.0.0/empty should be compatible")
	}
	if !majorVersionsCompatible("1.2.3", "1.9.0") {
		t.Errorf("same-major should be compatible")
	}
	if majorVersionsCompatible("1.2.3", "2.0.0") {
		t.Errorf("different-major should be incompatible")
	}
}

// TestUntar_RejectsUnknownEntry — untar surfaces a descriptive error
// when a tar entry isn't one of the expected names.
func TestUntar_RejectsUnknownEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, "evil.bin", []byte("nope")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	if _, _, err := untar(&buf); err == nil {
		t.Errorf("untar with unknown entry returned nil err")
	}
}

// TestUntar_RejectsMissingMeta — the archive must contain meta.json.
func TestUntar_RejectsMissingMeta(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "snapshot.bin", []byte("snap")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if _, _, err := untar(&buf); err == nil {
		t.Errorf("untar with no meta.json returned nil err")
	}
}

// TestUntar_RejectsMissingSnapshot — and snapshot.bin.
func TestUntar_RejectsMissingSnapshot(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if _, _, err := untar(&buf); err == nil {
		t.Errorf("untar with no snapshot.bin returned nil err")
	}
}

// TestUntar_RejectsCorruptGzip — the gzip header must parse before we
// even start reading tar entries.
func TestUntar_RejectsCorruptGzip(t *testing.T) {
	if _, _, err := untar(bytes.NewReader([]byte("not gzip"))); err == nil {
		t.Errorf("untar with corrupt gzip returned nil err")
	}
}

// TestReadMeta_ParsesEmbeddedMeta — round-trips an archive's meta.json
// without touching disk.
func TestReadMeta_ParsesEmbeddedMeta(t *testing.T) {
	const metaJSON = `{"schema_version":1,"cluster_id":"cx","snapshot_index":42,"snapshot_term":3,"jaco_version":"0.0.1","taken_at":"2025-01-01T00:00:00Z","leader_at_snapshot":"leader-1"}`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", []byte(metaJSON)); err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, "snapshot.bin", []byte("snap")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	meta, err := ReadMeta(&buf)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.ClusterID != "cx" || meta.SnapshotIndex != 42 || meta.JacoVersion != "0.0.1" {
		t.Errorf("meta mismatch: %+v", meta)
	}
}

// TestReadMeta_RejectsMalformedJSON — meta.json present but body is
// junk; ReadMeta surfaces a parse error.
func TestReadMeta_RejectsMalformedJSON(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := writeTarFile(tw, "snapshot.bin", []byte("s")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if _, err := ReadMeta(&buf); err == nil {
		t.Errorf("ReadMeta on malformed JSON returned nil err")
	}
}

// TestExport_RejectsMissingFields — every required ExportOptions
// field surfaces a descriptive error.
func TestExport_RejectsMissingFields(t *testing.T) {
	if err := Export(ExportOptions{}); err == nil || err.Error() != "Raft is required" {
		t.Errorf("no raft: err = %v, want \"Raft is required\"", err)
	}
}

// TestImport_RejectsMissingFields — symmetric guard on the Import side.
func TestImport_RejectsMissingFields(t *testing.T) {
	if err := Import(ImportOptions{}); err == nil || err.Error() != "DataDir is required" {
		t.Errorf("no DataDir: err = %v", err)
	}
	if err := Import(ImportOptions{DataDir: "/tmp"}); err == nil || err.Error() != "Reader is required" {
		t.Errorf("no Reader: err = %v", err)
	}
	if err := Import(ImportOptions{DataDir: "/tmp", Reader: bytes.NewReader(nil)}); err == nil || err.Error() != "LocalID is required" {
		t.Errorf("no LocalID: err = %v", err)
	}
}
