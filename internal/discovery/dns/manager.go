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
	done      chan struct{} // closed on intentional shutdown; stops bind retries
}

// stop signals the retry loops to exit and shuts the listeners down. Safe to
// call once per entry (entries are removed from the map under m.mu).
func (e *listenerEntry) stop() {
	close(e.done)
	_ = e.udp.Shutdown()
	_ = e.tcp.Shutdown()
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

	m.reconcileSubnets(ctx)
	m.refreshServiceMaps()

	for {
		select {
		case <-ctx.Done():
			m.shutdownAll()
			return nil
		case <-subnetSub.Events():
			m.reconcileSubnets(ctx)
		case <-obsSub.Events():
			m.refreshServiceMaps()
		}
	}
}

func (m *Manager) reconcileSubnets(ctx context.Context) {
	live := map[string]*pb.Subnet{}
	for _, sn := range m.State.Subnets.List() {
		live[scopeKey(sn.GetDeployment(), sn.GetNetwork())] = sn
	}
	m.mu.Lock()
	for k, l := range m.listeners {
		if _, ok := live[k]; !ok {
			l.stop()
			delete(m.listeners, k)
		}
	}
	m.mu.Unlock()
	for _, sn := range live {
		m.ensure(ctx, sn)
	}
}

func (m *Manager) ensure(ctx context.Context, sn *pb.Subnet) {
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
	done := make(chan struct{})

	go m.serveWithRetry(ctx, udp, done, sn.GetDeployment(), sn.GetNetwork(), "UDP")
	go m.serveWithRetry(ctx, tcp, done, sn.GetDeployment(), sn.GetNetwork(), "TCP")

	m.mu.Lock()
	m.listeners[key] = &listenerEntry{scope: scope, responder: resp, udp: udp, tcp: tcp, done: done}
	m.mu.Unlock()
}

// serveWithRetry runs ListenAndServe, retrying with backoff (200ms→5s) when
// the bind fails — the bridge gateway IP may not be on the interface yet when
// the subnet watch first fires (issue #28). Exits when the bind succeeds and
// is later Shutdown, when done is closed (listener removed), or on ctx cancel.
func (m *Manager) serveWithRetry(ctx context.Context, srv *dns.Server, done <-chan struct{}, dep, network, proto string) {
	const maxBackoff = 5 * time.Second
	backoff := 200 * time.Millisecond
	logged := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		default:
		}
		err := srv.ListenAndServe()
		// Distinguish an intentional Shutdown (done closed) from a bind error.
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
		}
		if err == nil {
			return // clean shutdown without done set (shouldn't happen, but stop)
		}
		if !logged {
			m.Logger.Printf("dns.Manager: %s/%s %s listen failed (%v); retrying until the bridge gateway is up", dep, network, proto, err)
			logged = true
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (m *Manager) refreshServiceMaps() {
	// Build per-scope service maps from ReplicasObserved filtered to RUNNING
	// + last_health_at < 10s. The IP for a scope comes from the per-network
	// detail key Details["ip.<dockerNetwork>"] the health watcher writes
	// (issue #28), so a multi-homed replica contributes the right IP per
	// scope. Locality: if any healthy replica of a service runs on this host,
	// only its local IP(s) are served — avoiding the cross-host trombone;
	// otherwise every cluster-wide IP is served (the responder shuffles).
	now := time.Now()
	type svcIPs struct{ local, all []net.IP }
	collected := map[string]map[string]*svcIPs{} // scopeKey -> service -> ips
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
		for _, network := range networksOfService(m.State, rep.GetDeployment(), rep.GetService()) {
			dockerNet := bridge.DockerNetworkName(rep.GetDeployment(), network)
			ip := net.ParseIP(obs.GetDetails()["ip."+dockerNet])
			if ip == nil {
				continue
			}
			key := scopeKey(rep.GetDeployment(), network)
			if collected[key] == nil {
				collected[key] = map[string]*svcIPs{}
			}
			s := collected[key][rep.GetService()]
			if s == nil {
				s = &svcIPs{}
				collected[key][rep.GetService()] = s
			}
			s.all = append(s.all, ip)
			if obs.GetHost() == m.Hostname {
				s.local = append(s.local, ip)
			}
		}
	}

	mapsByScope := map[string]ServiceMap{}
	for key, svcs := range collected {
		sm := ServiceMap{}
		for svc, ips := range svcs {
			if len(ips.local) > 0 {
				sm[svc] = ips.local
			} else {
				sm[svc] = ips.all
			}
		}
		mapsByScope[key] = sm
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
		l.stop()
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
