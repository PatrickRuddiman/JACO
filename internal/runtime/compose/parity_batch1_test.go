package compose_test

import (
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestValidate_AcceptsBatch1Fields — issues #114, #117, #118: the 16 new
// service-level keys must all be accepted by the closed-field validator.
// One sub-test per field so a regression names the exact key that broke.
func TestValidate_AcceptsBatch1Fields(t *testing.T) {
	cases := map[string]string{
		// #114 shutdown semantics
		"stop_signal":       "    stop_signal: SIGINT\n",
		"stop_grace_period": "    stop_grace_period: 30s\n",

		// #117 trivial passthroughs
		"extra_hosts": "    extra_hosts: [\"host1:1.2.3.4\"]\n",
		"dns":         "    dns: [\"1.1.1.1\"]\n",
		"dns_search":  "    dns_search: [\"example.com\"]\n",
		"dns_opt":     "    dns_opt: [\"ndots:2\"]\n",
		"init":        "    init: true\n",
		"shm_size":    "    shm_size: 64m\n",
		"hostname":    "    hostname: web\n",
		"domainname":  "    domainname: example.com\n",

		// #118 namespace knobs
		"ipc":           "    ipc: shareable\n",
		"pid":           "    pid: host\n",
		"uts":           "    uts: host\n",
		"userns_mode":   "    userns_mode: host\n",
		"cgroup":        "    cgroup: private\n",
		"cgroup_parent": "    cgroup_parent: /custom.slice\n",
	}
	for field, line := range cases {
		t.Run(field, func(t *testing.T) {
			body := []byte("services:\n  web:\n    image: nginx:1.27\n" + line + "networks:\n  default: {}\n")
			if err := compose.Validate(body); err != nil {
				t.Fatalf("Validate(%s): unexpected err: %v", field, err)
			}
		})
	}
}

// TestToContainerSpec_ProjectsBatch1Fields — round-trip the new fields from a
// compose ServiceConfig through ToContainerSpec onto the runtime ContainerSpec.
// Catches a missing projection line in spec.go.
func TestToContainerSpec_ProjectsBatch1Fields(t *testing.T) {
	graceDur := types.Duration(45 * time.Second)
	initTrue := true
	svc := types.ServiceConfig{
		Image:           "nginx:1.27",
		StopSignal:      "SIGINT",
		StopGracePeriod: &graceDur,
		Hostname:        "web",
		DomainName:      "example.com",
		ExtraHosts:      types.HostsList{"host1": []string{"1.2.3.4"}},
		DNS:             types.StringList{"1.1.1.1"},
		DNSSearch:       types.StringList{"example.com"},
		DNSOpts:         []string{"ndots:2"},
		Init:            &initTrue,
		ShmSize:         types.UnitBytes(64 * 1024 * 1024),
		Ipc:             "shareable",
		Pid:             "host",
		Uts:             "host",
		UserNSMode:      "host",
		Cgroup:          "private",
		CgroupParent:    "/custom.slice",
	}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{
		ClusterID: "c", Deployment: "d", Service: "web", ReplicaID: "r", ReplicaIndex: 0,
	})

	if spec.StopSignal != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", spec.StopSignal)
	}
	if spec.StopGracePeriodSeconds == nil || *spec.StopGracePeriodSeconds != 45 {
		t.Errorf("StopGracePeriodSeconds = %v, want *45", spec.StopGracePeriodSeconds)
	}
	if spec.Hostname != "web" {
		t.Errorf("Hostname = %q, want web", spec.Hostname)
	}
	if spec.Domainname != "example.com" {
		t.Errorf("Domainname = %q, want example.com", spec.Domainname)
	}
	if len(spec.ExtraHosts) != 1 || spec.ExtraHosts[0] != "host1:1.2.3.4" {
		t.Errorf("ExtraHosts = %v, want [host1:1.2.3.4]", spec.ExtraHosts)
	}
	if len(spec.DNS) != 1 || spec.DNS[0] != "1.1.1.1" {
		t.Errorf("DNS = %v, want [1.1.1.1]", spec.DNS)
	}
	if len(spec.DNSSearch) != 1 || spec.DNSSearch[0] != "example.com" {
		t.Errorf("DNSSearch = %v, want [example.com]", spec.DNSSearch)
	}
	if len(spec.DNSOptions) != 1 || spec.DNSOptions[0] != "ndots:2" {
		t.Errorf("DNSOptions = %v, want [ndots:2]", spec.DNSOptions)
	}
	if spec.Init == nil || *spec.Init != true {
		t.Errorf("Init = %v, want *true", spec.Init)
	}
	if spec.ShmSizeBytes != 64*1024*1024 {
		t.Errorf("ShmSizeBytes = %d, want %d", spec.ShmSizeBytes, 64*1024*1024)
	}
	if spec.IpcMode != "shareable" {
		t.Errorf("IpcMode = %q, want shareable", spec.IpcMode)
	}
	if spec.PidMode != "host" {
		t.Errorf("PidMode = %q, want host", spec.PidMode)
	}
	if spec.UTSMode != "host" {
		t.Errorf("UTSMode = %q, want host", spec.UTSMode)
	}
	if spec.UsernsMode != "host" {
		t.Errorf("UsernsMode = %q, want host", spec.UsernsMode)
	}
	if spec.CgroupnsMode != "private" {
		t.Errorf("CgroupnsMode = %q, want private", spec.CgroupnsMode)
	}
	if spec.CgroupParent != "/custom.slice" {
		t.Errorf("CgroupParent = %q, want /custom.slice", spec.CgroupParent)
	}
}

// TestToContainerSpec_BatchFieldZeroValuesStayZero — when compose declares
// nothing, the projected spec carries zero values so the lifecycle builder
// emits docker's defaults (no override).
func TestToContainerSpec_BatchFieldZeroValuesStayZero(t *testing.T) {
	svc := types.ServiceConfig{Image: "nginx:1.27"}
	spec := compose.ToContainerSpec(svc, compose.SpecOptions{ReplicaID: "r"})

	if spec.StopSignal != "" {
		t.Errorf("StopSignal = %q, want empty", spec.StopSignal)
	}
	if spec.StopGracePeriodSeconds != nil {
		t.Errorf("StopGracePeriodSeconds = %v, want nil", spec.StopGracePeriodSeconds)
	}
	if spec.Init != nil {
		t.Errorf("Init = %v, want nil", spec.Init)
	}
	if spec.ShmSizeBytes != 0 {
		t.Errorf("ShmSizeBytes = %d, want 0", spec.ShmSizeBytes)
	}
	if spec.Hostname != "" || spec.Domainname != "" {
		t.Errorf("Hostname/Domainname = %q/%q, want empty/empty", spec.Hostname, spec.Domainname)
	}
	if spec.IpcMode != "" || spec.PidMode != "" || spec.UTSMode != "" || spec.UsernsMode != "" {
		t.Errorf("namespace modes leaked: ipc=%q pid=%q uts=%q userns=%q",
			spec.IpcMode, spec.PidMode, spec.UTSMode, spec.UsernsMode)
	}
	if spec.CgroupnsMode != "" || spec.CgroupParent != "" {
		t.Errorf("cgroup leaked: cgroup=%q parent=%q", spec.CgroupnsMode, spec.CgroupParent)
	}
}
