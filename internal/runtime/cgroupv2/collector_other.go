//go:build !linux

package cgroupv2

// Read always returns Ok=false on non-Linux builds — there's no
// cgroup v2 hierarchy to read from. The rebalancer treats this as
// "no data this cycle" and skips the node entirely; nothing else in
// JACO depends on the sample. We keep the type compiling so dev
// workstations (macOS, Windows under WSL) build the daemon binary
// for tests and tooling.
func (c *Collector) Read() Sample {
	return Sample{Ok: false}
}
