package firewall_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

// fakeLister returns canned JSON for nft list.
type fakeLister struct {
	body []byte
	err  error
}

func (f *fakeLister) List(_ context.Context) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

// recordingApplier captures Apply invocations.
type recordingApplier struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingApplier) Apply(_ context.Context, ruleset string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ruleset)
	return r.err
}

func (r *recordingApplier) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// recordingAudit captures audit events.
type recordingAudit struct {
	mu     sync.Mutex
	events []auditEntry
}

type auditEntry struct {
	code    string
	details map[string]string
}

func (r *recordingAudit) fn() firewall.AuditFn {
	return func(_ context.Context, code string, details map[string]string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, auditEntry{code: code, details: details})
		return nil
	}
}

func (r *recordingAudit) Codes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.code
	}
	return out
}

// recordingStatus captures NodeStatusUpdate calls.
type recordingStatus struct {
	mu      sync.Mutex
	updates []statusUpdate
}

type statusUpdate struct {
	status string
	reason string
}

func (r *recordingStatus) fn() firewall.IsolationStatusFn {
	return func(_ context.Context, status, reason string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.updates = append(r.updates, statusUpdate{status: status, reason: reason})
		return nil
	}
}

func (r *recordingStatus) Updates() []statusUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]statusUpdate, len(r.updates))
	copy(out, r.updates)
	return out
}

// goodInput returns a RuleInput shape the tests reuse for both Render and
// SelfTest.
func goodInput() firewall.RuleInput {
	return firewall.RuleInput{
		Subnets: []firewall.Subnet{{Deployment: "sample", Network: "frontend", CIDR: "10.244.5.0/24"}},
	}
}

// validJSON is `nft -j list` output that matches goodInput (3 chains + 1
// set).
const validJSON = `{"nftables":[
	{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"drop"}},
	{"chain":{"family":"inet","table":"jaco","name":"input","hook":"input","prio":0,"policy":"drop"}},
	{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
	{"set":{"family":"inet","table":"jaco","name":"dep_net_sample_frontend","type":"ipv4_addr"}}
]}`

// driftedJSON is missing the input chain (drift simulation).
const driftedJSON = `{"nftables":[
	{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"drop"}},
	{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
	{"set":{"family":"inet","table":"jaco","name":"dep_net_sample_frontend","type":"ipv4_addr"}}
]}`

func TestReconcile_HappyPathNoDriftSilent(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       &fakeLister{body: []byte(validJSON)},
		Applier:      apl,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if apl.Count() != 0 {
		t.Errorf("Apply called %d times on no-drift; want 0", apl.Count())
	}
	if len(aud.Codes()) != 0 {
		t.Errorf("audit events on no-drift: %v", aud.Codes())
	}
}

func TestReconcile_AssertsSNATExemptEachTick(t *testing.T) {
	var gotPool string
	calls := 0
	r := &firewall.Reconciler{
		Lister:       &fakeLister{body: []byte(validJSON)},
		Applier:      &recordingApplier{},
		Audit:        (&recordingAudit{}).fn(),
		UpdateStatus: (&recordingStatus{}).fn(),
		Render:       goodInput,
		Pool:         "10.244.0.0/16",
		EnsureSNAT: func(_ context.Context, pool string) error {
			calls++
			gotPool = pool
			return nil
		},
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if calls != 1 {
		t.Errorf("EnsureSNAT called %d times, want 1", calls)
	}
	if gotPool != "10.244.0.0/16" {
		t.Errorf("EnsureSNAT pool = %q, want 10.244.0.0/16", gotPool)
	}
}

func TestReconcile_DriftDetectedReappliesAndAudits(t *testing.T) {
	// The AC: when nft list shows drift, the reconciler re-renders + applies
	// and writes an ISOLATION_RULESET_RECONCILED audit event.
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       &fakeLister{body: []byte(driftedJSON)},
		Applier:      apl,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if apl.Count() != 1 {
		t.Errorf("Apply called %d times; want 1", apl.Count())
	}
	codes := aud.Codes()
	if len(codes) != 1 || codes[0] != "ISOLATION_RULESET_RECONCILED" {
		t.Errorf("audit codes = %v, want [ISOLATION_RULESET_RECONCILED]", codes)
	}
}

func TestReconcile_ApplyFailureFlipsIsolationUnavailable(t *testing.T) {
	apl := &recordingApplier{err: errors.New("nftables: parse error")}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       &fakeLister{body: []byte(driftedJSON)},
		Applier:      apl,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	err := r.Tick(context.Background())
	if err == nil {
		t.Fatalf("expected error from Tick")
	}
	updates := stat.Updates()
	if len(updates) != 1 || updates[0].status != "isolation_unavailable" {
		t.Errorf("status updates = %v, want [isolation_unavailable]", updates)
	}
}

func TestReconcile_RecoveryFromIsolationUnavailableEmitsReady(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	// Start with drifted state + Apply error to set the degraded flag.
	r := &firewall.Reconciler{
		Lister:       &fakeLister{body: []byte(driftedJSON)},
		Applier:      &recordingApplier{err: errors.New("transient")},
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	_ = r.Tick(context.Background())
	// Swap to healthy state + working applier.
	r.Lister = &fakeLister{body: []byte(validJSON)}
	r.Applier = apl
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	updates := stat.Updates()
	// Expect updates: [isolation_unavailable, ready].
	if len(updates) != 2 {
		t.Fatalf("status updates = %v, want exactly 2", updates)
	}
	if updates[1].status != "ready" {
		t.Errorf("second update = %q, want ready", updates[1].status)
	}
	codes := aud.Codes()
	if len(codes) == 0 || codes[len(codes)-1] != "ISOLATION_RULESET_RECONCILED" {
		t.Errorf("expected ISOLATION_RULESET_RECONCILED audit on recovery; got %v", codes)
	}
}

// TestReconcile_MissingTableBootstraps — a list error means `table inet jaco`
// doesn't exist yet; the reconciler must apply (create) it rather than bail,
// and a successful apply must NOT mark the node isolation_unavailable.
func TestReconcile_MissingTableBootstraps(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       &fakeLister{err: errors.New("nft: No such file or directory")},
		Applier:      apl,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick should bootstrap the table, got: %v", err)
	}
	if apl.Count() != 1 {
		t.Errorf("Apply called %d times; want 1 (bootstrap)", apl.Count())
	}
	for _, u := range stat.Updates() {
		if u.status == "isolation_unavailable" {
			t.Errorf("missing table should not mark isolation_unavailable: %+v", stat.Updates())
		}
	}
}

// TestReconcile_ListAndApplyErrorMarksDegraded — when nft is genuinely broken
// (both list AND apply fail) the node flips to isolation_unavailable.
func TestReconcile_ListAndApplyErrorMarksDegraded(t *testing.T) {
	apl := &recordingApplier{err: errors.New("nft -f: syntax error")}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       &fakeLister{err: errors.New("nft exec failed")},
		Applier:      apl,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	if err := r.Tick(context.Background()); err == nil {
		t.Fatalf("expected error when both list and apply fail")
	}
	found := false
	for _, u := range stat.Updates() {
		if u.status == "isolation_unavailable" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected isolation_unavailable status; got %+v", stat.Updates())
	}
}

// silence unused import warning
var _ = fmt.Sprintf
