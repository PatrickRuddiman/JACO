package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// jacoServiceUnit is the systemd unit name installed by the deb/rpm
// (build/jaco.service, WantedBy=multi-user.target). `systemctl enable jaco`
// is the invocation the postinstall message and the testbed bootstrap use.
const jacoServiceUnit = "jaco"

// systemdEnabler enables the jaco service so a cluster-committed node survives
// reboot. It is a package var so tests can stub it without touching real
// systemd; production wiring points it at enableJacoService.
var systemdEnabler = enableJacoService

// enableJacoService runs `systemctl enable jaco` after a successful
// `cluster init` / `node join` so the daemon comes back up on reboot — those
// commands are the operator's "this node is now a cluster member" commitment
// boundary, which is exactly where the deb's deliberately-disabled posture
// stops being the right answer (issue #151).
//
// It is best-effort by design: the cluster is already committed by the time we
// get here, so neither a missing systemctl nor an enable failure aborts the
// command — both would only leave the operator with a confusing error and no
// clean remedy. Instead:
//
//   - No systemctl on PATH (alpine/apk, containers, dev machines): a friendly
//     note and an idempotent no-op.
//   - `systemctl enable` fails: a warning pointing the operator at the manual
//     fix.
//
// We use `enable` (not `enable --now`): the daemon is already running, so we
// only need persistence across reboot, not a restart.
func enableJacoService(out io.Writer) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(out, "Note: systemctl not found; skipping auto-enable. Ensure jacod starts on boot via your service manager so this node rejoins after a reboot.")
		return
	}
	cmd := exec.Command("systemctl", "enable", jacoServiceUnit)
	if combined, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "warning: `systemctl enable %s` failed: %v: %s\n", jacoServiceUnit, err, strings.TrimSpace(string(combined)))
		fmt.Fprintf(out, "Run `sudo systemctl enable %s` manually so this node rejoins the cluster after a reboot.\n", jacoServiceUnit)
		return
	}
	fmt.Fprintf(out, "Enabled %s.service to start on boot — this node now survives reboot.\n", jacoServiceUnit)
}
