package cgroupv2

import (
	"sync"
	"time"
)

// Sample is one cycle's worth of normalised utilisation. Each field is
// in [0, 1] (1.0 = saturated). Ok reports whether the collector could
// read the underlying files — false on non-Linux builds, missing
// cgroup v2 mount, unreadable files, or before the first delta sample
// has built up. Callers that need the rebalancer's PressureSource
// shape adapt via the daemon-side state-backed source; this package
// stays self-contained.
type Sample struct {
	CPU    float64
	Memory float64
	Ok     bool
}

// Collector caches the last good sample and the previous raw counters
// so the next Read() can compute a delta without the caller having to
// remember anything. Zero value is usable.
//
// Safe for concurrent Read calls; a single in-flight read is
// serialised so two concurrent callers don't double-advance the
// internal counter snapshot.
type Collector struct {
	// CPUCount overrides the runtime CPU count used to normalise
	// cpu.stat. Tests set it to a fixed number; production leaves
	// it zero and the collector reads runtime.NumCPU() at
	// construction time.
	CPUCount int

	// MemTotalBytes overrides the memory-capacity denominator used
	// when cgroup memory.max is "max" (the common case on bare
	// metal / VM root cgroup). Tests inject a value; production
	// reads /proc/meminfo at construction.
	MemTotalBytes uint64

	// Now lets tests inject a deterministic clock. Defaults to
	// time.Now.
	Now func() time.Time

	// Root is the cgroup v2 mountpoint. Defaults to
	// "/sys/fs/cgroup". Tests point this at a tmpdir with fake
	// cpu.stat / memory.current files.
	Root string

	once sync.Once
	mu   sync.Mutex
	prev struct {
		usageUsec uint64
		at        time.Time
		valid     bool
	}
	lastGood Sample
}
