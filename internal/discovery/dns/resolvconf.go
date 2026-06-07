package dns

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

// ReadHostResolvers parses an /etc/resolv.conf-format file and returns
// its `nameserver` entries, normalized to host:port (default 53).
// Entries that would loop back into JACO are silently filtered:
//
//   - 127.0.0.11 — Docker's embedded resolver. Containers reach us
//     THROUGH this; configuring it as our upstream would loop.
//   - 10.244.*.1 — every JACO bridge gateway. Likewise.
//
// Returns an empty slice (NOT an error) when the file exists but has
// no usable nameserver lines — the caller can decide whether that's
// a startup warning or an outright failure based on whether
// `dns.forwarders` is also unset.
//
// Issue #165 — used by the daemon's DNS subsystem to default the
// upstream list when the operator hasn't specified one explicitly.
func ReadHostResolvers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Strip trailing comment so `nameserver 1.1.1.1 # comment`
		// works the same way most resolvers handle it.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		raw := fields[1]
		host, _, _ := net.SplitHostPort(ensurePort(raw, "53"))
		ip := net.ParseIP(host)
		if ip == nil {
			// nameserver lines historically allow only literal IPs; a
			// non-IP value is malformed in resolv.conf. Skip silently —
			// don't fail the whole parse for one stray line.
			continue
		}
		// Filter loops: Docker's embedded resolver and our own bridge
		// gateways. validateUpstreams rejects these for operator-supplied
		// upstreams; here we silently drop them so a host whose resolv.conf
		// happens to list a bridge gateway boots cleanly rather than
		// disabling DNS entirely.
		v4 := ip.To4()
		if v4 != nil && v4[0] == 127 && v4[3] == 11 {
			continue
		}
		if isBridgeGatewayIP(ip) {
			continue
		}
		out = append(out, ensurePort(raw, "53"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}
