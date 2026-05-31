//go:build linux

package cgroupv2

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Read returns the current Sample. The first call only seeds the
// counter snapshot and returns Ok=false; the second and subsequent
// calls compute a real delta. Memory is a point-in-time ratio so it's
// usable on the first call too — but to keep the (CPU, Memory) tuple
// internally consistent the collector reports Ok=false until both
// dimensions have data.
//
// On any read error the collector returns the last good sample with
// Ok=false (so the caller falls through to the rebalancer's "no data
// this cycle" path). Errors are intentionally swallowed — the
// rebalancer treats absence as silence, not as a hard fault.
func (c *Collector) Read() Sample {
	c.once.Do(c.init)
	c.mu.Lock()
	defer c.mu.Unlock()

	cpu, cpuOk := c.readCPU()
	mem, memOk := c.readMemory()
	if !cpuOk || !memOk {
		out := c.lastGood
		out.Ok = false
		return out
	}
	s := Sample{CPU: cpu, Memory: mem, Ok: true}
	c.lastGood = s
	return s
}

func (c *Collector) init() {
	if c.Root == "" {
		c.Root = "/sys/fs/cgroup"
	}
	if c.CPUCount == 0 {
		c.CPUCount = runtime.NumCPU()
	}
	if c.MemTotalBytes == 0 {
		c.MemTotalBytes = readMemTotal()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// readCPU returns the normalised utilisation across the wall-time
// window between the last successful sample and now. The first call
// returns ok=false; the second returns the real delta. cpu.stat's
// usage_usec is cumulative CPU-microseconds (summed across all CPUs),
// so saturation is `delta_usec / (wall_seconds * cpu_count * 1e6)`.
func (c *Collector) readCPU() (float64, bool) {
	usage, err := readUsageUsec(c.Root + "/cpu.stat")
	if err != nil {
		return 0, false
	}
	now := c.Now()
	defer func() {
		c.prev.usageUsec = usage
		c.prev.at = now
		c.prev.valid = true
	}()
	if !c.prev.valid {
		return 0, false
	}
	wall := now.Sub(c.prev.at).Seconds()
	if wall <= 0 {
		return 0, false
	}
	deltaUsec := float64(usage - c.prev.usageUsec)
	ratio := deltaUsec / (wall * float64(c.CPUCount) * 1e6)
	return clamp01(ratio), true
}

// readMemory returns memory.current / capacity. Capacity is
// memory.max when it parses as a uint64; otherwise the host's
// /proc/meminfo MemTotal (covers the common "max" root cgroup).
func (c *Collector) readMemory() (float64, bool) {
	cur, err := readUint64File(c.Root + "/memory.current")
	if err != nil {
		return 0, false
	}
	capacity := c.MemTotalBytes
	if maxBytes, ok := readMemoryMax(c.Root + "/memory.max"); ok {
		capacity = maxBytes
	}
	if capacity == 0 {
		return 0, false
	}
	return clamp01(float64(cur) / float64(capacity)), true
}

func readUsageUsec(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		const key = "usage_usec "
		if strings.HasPrefix(line, key) {
			return strconv.ParseUint(strings.TrimSpace(line[len(key):]), 10, 64)
		}
	}
	return 0, errors.New("cgroupv2: usage_usec not in cpu.stat")
}

func readUint64File(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readMemoryMax returns the parsed memory.max value. The file holds
// either "max" (no cap configured, common on root cgroup) or a byte
// count. "max" returns ok=false so the caller falls back to
// /proc/meminfo.
func readMemoryMax(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readMemTotal parses /proc/meminfo's MemTotal (kB) into bytes.
// Returns 0 on any error — the collector then reports memOk=false.
func readMemTotal() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16383128 kB"
		var v uint64
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &v); err != nil {
			return 0
		}
		return v * 1024
	}
	return 0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
