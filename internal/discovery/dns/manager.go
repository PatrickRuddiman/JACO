package dns

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ListenPort is the per-bridge DNS port. 53 is the conventional choice
// containers' resolv.conf will reach.
const ListenPort = 53

// Manager owns one DNS listener per (deployment, network) bridge on the
// local node. It subscribes to state.Subnets to spawn / stop listeners
// as bridges come and go, and to state.ReplicasObserved to update the
// per-responder ServiceMap.
//
// Listener binding to port 53 needs CAP_NET_BIND_SERVICE. When the host
// doesn't grant it, ListenAndServe errors get logged once and that
// scope's resolution is disabled. The daemon proceeds; intra-bridge
// resolution falls back to docker's built-in DNS.
type Manager struct {
	State    *state.State
	Brokers  *watch.Registry
	Logger   *log.Logger
	Hostname string

	mu        sync.Mutex
	listeners map[string]*listenerEntry // keyed by scopeKey(deployment, network)
}

type listenerEntry struct {
	scope     Scope
	responder *Responder
	udp       *dns.Server
	tcp       *dns.Server
}

// Run blocks until ctx is cancelled. Spawns / stops per-bridge listeners
// as state.Subnets changes; refreshes ServiceMaps when ReplicasObserved
// changes.
func (m *Manager) Run(ctx context.Context) error {
	if m.Logger == nil {
		m.Logger = log.Default()
	}
	if m.listeners == nil {
		m.listeners = map[string]*listenerEntry{}
	}

	subnetSub := m.Brokers.Subnets.Subscribe()
	defer subnetSub.Cancel()
	obsSub := m.Brokers.ReplicasObserved.Subscribe()
	defer obsSub.Cancel()

	m.reconcileSubnets()
	m.refreshServiceMaps()

	for {
		select {
		case <-ctx.Done():
			m.shutdownAll()
			return nil
		case <-subnetSub.Events():
			m.reconcileSubnets()
		case <-obsSub.Events():
			m.refreshServiceMaps()
		}
	}
}

func (m *Manager) reconcileSubnets() {
	live := map[string]*pb.Subnet{}
	for _, sn := range m.State.Subnets.List() {
		live[scopeKey(sn.GetDeployment(), sn.GetNetwork())] = sn
	}
	m.mu.Lock()
	for k, l := range m.listeners {
		if _, ok := live[k]; !ok {
			_ = l.udp.Shutdown()
			_ = l.tcp.Shutdown()
			delete(m.listeners, k)
		}
	}
	m.mu.Unlock()
	for _, sn := range live {
		m.ensure(sn)
	}
}

func (m *Manager) ensure(sn *pb.Subnet) {
	key := scopeKey(sn.GetDeployment(), sn.GetNetwork())
	m.mu.Lock()
	if _, ok := m.listeners[key]; ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	gw, err := bridge.GatewayIP(sn.GetCidr())
	if err != nil {
		m.Logger.Printf("dns.Manager: subnet %s/%s bad CIDR %q: %v", sn.GetDeployment(), sn.GetNetwork(), sn.GetCidr(), err)
		return
	}
	scope := Scope{Deployment: sn.GetDeployment(), Network: sn.GetNetwork()}
	resp := New(scope, ServiceMap{}, defaultForwarder{})

	addr := fmt.Sprintf("%s:%d", gw, ListenPort)
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: dnsHandlerFunc(resp.Handle)}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: dnsHandlerFunc(resp.Handle)}

	go func() {
		if err := udp.ListenAndServe(); err != nil {
			m.Logger.Printf("dns.Manager: %s/%s UDP listen failed (%v); responder disabled", sn.GetDeployment(), sn.GetNetwork(), err)
		}
	}()
	go func() {
		if err := tcp.ListenAndServe(); err != nil {
			m.Logger.Printf("dns.Manager: %s/%s TCP listen failed (%v)", sn.GetDeployment(), sn.GetNetwork(), err)
		}
	}()

	m.mu.Lock()
	m.listeners[key] = &listenerEntry{scope: scope, responder: resp, udp: udp, tcp: tcp}
	m.mu.Unlock()
}

func (m *Manager) refreshServiceMaps() {
	// Build per-scope service maps from ReplicasObserved filtered to
	// RUNNING + last_health_at < 10s. IPs come from the runtime's
	// Details["ip"] entry, written by the discovery/runtime_attach
	// slice when it joins a replica to a network.
	now := time.Now()
	mapsByScope := map[string]ServiceMap{}
	for _, obs := range m.State.ReplicasObserved.List() {
		if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
			continue
		}
		if lh := obs.GetLastHealthAt(); lh == nil || now.Sub(lh.AsTime()) > 10*time.Second {
			continue
		}
		rep, ok := m.State.ReplicasDesired.Get(obs.GetId())
		if !ok {
			continue
		}
		ipStr := obs.GetDetails()["ip"]
		if ipStr == "" {
			continue
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		for _, network := range networksOfService(m.State, rep.GetDeployment(), rep.GetService()) {
			key := scopeKey(rep.GetDeployment(), network)
			if _, ok := mapsByScope[key]; !ok {
				mapsByScope[key] = ServiceMap{}
			}
			mapsByScope[key][rep.GetService()] = append(mapsByScope[key][rep.GetService()], ip)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for key, l := range m.listeners {
		if sm, ok := mapsByScope[key]; ok {
			l.responder.SetServices(sm)
		} else {
			l.responder.SetServices(ServiceMap{})
		}
	}
}

func (m *Manager) shutdownAll() {
	m.mu.Lock()
	for k, l := range m.listeners {
		_ = l.udp.Shutdown()
		_ = l.tcp.Shutdown()
		delete(m.listeners, k)
	}
	m.mu.Unlock()
}

func scopeKey(deployment, network string) string {
	return deployment + "/" + network
}

func networksOfService(st *state.State, deployment, service string) []string {
	dep, ok := st.Deployments.Get(deployment)
	if !ok {
		return nil
	}
	for _, svc := range dep.GetServices() {
		if svc.GetName() == service {
			return svc.GetNetworks()
		}
	}
	return nil
}

type defaultForwarder struct{}

func (defaultForwarder) LookupHost(host string) ([]net.IP, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			out = append(out, ip)
		}
	}
	return out, nil
}

type dnsHandlerFunc func(*dns.Msg) *dns.Msg

func (f dnsHandlerFunc) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	resp := f(r)
	if resp != nil {
		_ = w.WriteMsg(resp)
	}
}
