//go:build linux

package cgroupv2_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/runtime/cgroupv2"
)

// writeFakeRoot writes the cgroup files the collector reads. Memory
// max is configurable so tests can exercise both the explicit-limit
// and "max" fallback paths.
func writeFakeRoot(t *testing.T, usageUsec uint64, memCurrent uint64, memMax string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec "+itoa(usageUsec)+"\nuser_usec 0\nsystem_usec 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.current"),
		[]byte(itoa(memCurrent)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.max"),
		[]byte(memMax+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func itoa(v uint64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	return string(buf)
}

// TestCollector_FirstCallSeedsThenDeltaReports — the first Read seeds
// the prev counters and returns Ok=false; the second computes a real
// CPU delta from the wall-time gap.
func TestCollector_FirstCallSeedsThenDeltaReports(t *testing.T) {
	root := writeFakeRoot(t, 0, 512<<20, "1073741824") // 512MB / 1GB
	now := time.Unix(1_700_000_000, 0)
	c := &cgroupv2.Collector{
		Root:          root,
		CPUCount:      4,
		MemTotalBytes: 1 << 30,
		Now:           func() time.Time { return now },
	}

	s := c.Read()
	if s.Ok {
		t.Fatalf("first call must be !ok (seeds counters), got %+v", s)
	}

	// 1 second elapsed, 2 CPU-seconds consumed across 4 CPUs = 50%.
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec 2000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	s = c.Read()
	if !s.Ok {
		t.Fatalf("second call must be ok, got %+v", s)
	}
	if s.CPU < 0.49 || s.CPU > 0.51 {
		t.Errorf("CPU = %v; want ~0.50", s.CPU)
	}
	if s.Memory < 0.49 || s.Memory > 0.51 {
		t.Errorf("Memory = %v; want ~0.50", s.Memory)
	}
}

// TestCollector_MemoryMaxFallback — when memory.max == "max" the
// collector falls back to the injected MemTotalBytes.
func TestCollector_MemoryMaxFallback(t *testing.T) {
	root := writeFakeRoot(t, 0, 1<<30, "max")
	now := time.Unix(1_700_000_000, 0)
	c := &cgroupv2.Collector{
		Root:          root,
		CPUCount:      1,
		MemTotalBytes: 4 << 30, // 4GB
		Now:           func() time.Time { return now },
	}
	c.Read() // seed
	now = now.Add(time.Second)
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := c.Read()
	if !s.Ok {
		t.Fatalf("second call must be ok, got %+v", s)
	}
	if s.Memory < 0.24 || s.Memory > 0.26 {
		t.Errorf("Memory = %v; want ~0.25 (1GB / 4GB)", s.Memory)
	}
}

// TestCollector_SaturationClamps — over-100% utilisation (possible
// when the previous sample's wall-time underflows the CPU delta)
// clamps to 1.0, not to 12.7 or whatever the raw ratio computes to.
func TestCollector_SaturationClamps(t *testing.T) {
	root := writeFakeRoot(t, 0, 1<<20, "1073741824")
	now := time.Unix(1_700_000_000, 0)
	c := &cgroupv2.Collector{
		Root: root, CPUCount: 1, MemTotalBytes: 1 << 30,
		Now: func() time.Time { return now },
	}
	c.Read() // seed
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec 99000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second) // 99s of CPU in 1s of wall = 99x saturation
	s := c.Read()
	if s.CPU != 1.0 {
		t.Errorf("CPU = %v; want 1.0 (clamped)", s.CPU)
	}
}

// TestCollector_MissingFilesReturnsNotOk — a non-existent cgroup
// root makes Read return Ok=false rather than crashing. Covers the
// unprivileged-container / missing-mount case.
func TestCollector_MissingFilesReturnsNotOk(t *testing.T) {
	c := &cgroupv2.Collector{
		Root: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	if got := c.Read(); got.Ok {
		t.Errorf("missing root must be !ok, got %+v", got)
	}
}
