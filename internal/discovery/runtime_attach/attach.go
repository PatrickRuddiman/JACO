// Package runtime_attach is the state-driven lookup helper that returns the
// docker network names a (deployment, service) replica should be attached
// to. Used by orphan-reconcile (which doesn't have a ContainerSpec on hand)
// and any other caller that needs the per-service network list without
// re-parsing the compose file.
package runtime_attach

import (
	"fmt"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
)

// BridgesForService looks up (deployment, service) and returns the docker
// network names the replica must attach to. Returns the per-service network
// list as declared on `ServiceSpec.networks`; falls through to the implicit
// `_default` network when the service declares no networks.
//
// Returns an error when the deployment isn't found or the service isn't a
// member of it.
func BridgesForService(st *state.State, deployment, service string) ([]string, error) {
	dep, ok := st.Deployments.Get(deployment)
	if !ok {
		return nil, fmt.Errorf("BridgesForService: deployment %q not found", deployment)
	}
	for _, spec := range dep.GetServices() {
		if spec.GetName() != service {
			continue
		}
		nets := spec.GetNetworks()
		if len(nets) == 0 {
			return []string{bridge.DockerNetworkName(deployment, "_default")}, nil
		}
		out := make([]string, 0, len(nets))
		for _, n := range nets {
			out = append(out, bridge.DockerNetworkName(deployment, n))
		}
		return out, nil
	}
	return nil, fmt.Errorf("BridgesForService: service %q not found in deployment %q", service, deployment)
}
