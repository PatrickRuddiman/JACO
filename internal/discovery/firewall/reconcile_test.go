package firewall_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// recordingAudit captures audit events. Setting err makes every invocation
// return that error (still records the call).
type recordingAudit struct {
	mu     sync.Mutex
	events []auditEntry
	err    error
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
		return r.err
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

// recordingStatus captures NodeStatusUpdate calls. Setting err makes every
// invocation return that error (still records the call).
type recordingStatus struct {
	mu      sync.Mutex
	updates []statusUpdate
	err     error
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
		return r.err
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
// set). All base chains are policy accept — the shape Render emits (the
// no-host-disruption invariant); SelfTest must treat this as no-drift.
const validJSON = `{"nftables":[
	{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"accept"}},
	{"chain":{"family":"inet","table":"jaco","name":"input","hook":"input","prio":0,"policy":"accept"}},
	{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
	{"set":{"family":"inet","table":"jaco","name":"dep_net_sample_frontend","type":"ipv4_addr"}}
]}`

// driftedJSON is missing the input chain (drift simulation).
const driftedJSON = `{"nftables":[
	{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"accept"}},
	{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
	{"set":{"family":"inet","table":"jaco","name":"dep_net_sample_frontend","type":"ipv4_addr"}}
]}`

func TestReconcile_HappyPathNoDriftSilent(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(validJSON)}).List,
		Applier:      apl.Apply,
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
		Lister:       (&fakeLister{body: []byte(validJSON)}).List,
		Applier:      (&recordingApplier{}).Apply,
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

func TestReconcile_AssertsOverlayExemptEachTick(t *testing.T) {
	var gotPool string
	calls := 0
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(validJSON)}).List,
		Applier:      (&recordingApplier{}).Apply,
		Audit:        (&recordingAudit{}).fn(),
		UpdateStatus: (&recordingStatus{}).fn(),
		Render:       goodInput,
		Pool:         "10.244.0.0/16",
		EnsureOverlay: func(_ context.Context, pool string) error {
			calls++
			gotPool = pool
			return nil
		},
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if calls != 1 {
		t.Errorf("EnsureOverlay called %d times, want 1", calls)
	}
	if gotPool != "10.244.0.0/16" {
		t.Errorf("EnsureOverlay pool = %q, want 10.244.0.0/16", gotPool)
	}
}

func TestReconcile_OverlayExemptFailureAuditsButDoesNotFail(t *testing.T) {
	// A failure asserting the overlay exemption is best-effort: it audits
	// OVERLAY_EXEMPT_FAILED but must not fail the tick or flip isolation status.
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(validJSON)}).List,
		Applier:      (&recordingApplier{}).Apply,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
		Pool:         "10.244.0.0/16",
		EnsureOverlay: func(_ context.Context, _ string) error {
			return errors.New("iptables boom")
		},
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick should not fail on overlay-exempt error: %v", err)
	}
	if len(stat.Updates()) != 0 {
		t.Errorf("overlay-exempt failure flipped status: %v", stat.Updates())
	}
	found := false
	for _, c := range aud.Codes() {
		if c == "OVERLAY_EXEMPT_FAILED" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected OVERLAY_EXEMPT_FAILED audit, got %v", aud.Codes())
	}
}

func TestReconcile_DriftDetectedReappliesAndAudits(t *testing.T) {
	// The AC: when nft list shows drift, the reconciler re-renders + applies
	// and writes an ISOLATION_RULESET_RECONCILED audit event.
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(driftedJSON)}).List,
		Applier:      apl.Apply,
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

// TestReconcile_ColdBootEmptyDocAppliesRuleset covers the first-boot path:
// when the `inet jaco` table doesn't exist yet, NftList returns the empty
// document `{"nftables":[]}` (instead of a "table not found" error), and
// the reconciler must treat that as full drift and call Apply to create
// the table.
func TestReconcile_ColdBootEmptyDocAppliesRuleset(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(`{"nftables":[]}`)}).List,
		Applier:      apl.Apply,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if apl.Count() != 1 {
		t.Errorf("Apply called %d times on cold boot; want 1", apl.Count())
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
		Lister:       (&fakeLister{body: []byte(driftedJSON)}).List,
		Applier:      apl.Apply,
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

func TestReconcile_ApplyFailureLogsStatusUpdateError(t *testing.T) {
	// Regression for #45: when Apply fails AND UpdateStatus also fails,
	// the prior code swallowed the UpdateStatus error with `_ =`, masking
	// the real reason `nft list table inet jaco` stayed missing on a live
	// cluster. The reconciler must:
	//   1. log the apply error itself,
	//   2. log the UpdateStatus error (with apply error context),
	//   3. still return the wrapped applyErr to Loop.
	var logBuf bytes.Buffer
	applyErr := errors.New("nftables: parse error")
	statusErr := errors.New("raft: not leader")
	apl := &recordingApplier{err: applyErr}
	aud := &recordingAudit{}
	stat := &recordingStatus{err: statusErr}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(driftedJSON)}).List,
		Applier:      apl.Apply,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
		Logger:       slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	err := r.Tick(context.Background())
	if err == nil {
		t.Fatalf("expected error from Tick")
	}
	if !errors.Is(err, applyErr) {
		t.Errorf("Tick err = %v; want wrapped %v", err, applyErr)
	}
	updates := stat.Updates()
	if len(updates) != 1 || updates[0].status != "isolation_unavailable" {
		t.Errorf("status updates = %v, want [isolation_unavailable]", updates)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "apply ruleset failed") || !strings.Contains(logs, applyErr.Error()) {
		t.Errorf("expected apply-error log line containing %q, got: %s", applyErr.Error(), logs)
	}
	if !strings.Contains(logs, "UpdateStatus(isolation_unavailable) failed") || !strings.Contains(logs, statusErr.Error()) {
		t.Errorf("expected UpdateStatus-error log line containing %q, got: %s", statusErr.Error(), logs)
	}
}

func TestReconcile_RecoveryFromIsolationUnavailableEmitsReady(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	// Start with drifted state + Apply error to set the degraded flag.
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{body: []byte(driftedJSON)}).List,
		Applier:      (&recordingApplier{err: errors.New("transient")}).Apply,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	_ = r.Tick(context.Background())
	// Swap to healthy state + working applier.
	r.Lister = (&fakeLister{body: []byte(validJSON)}).List
	r.Applier = apl.Apply
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

func TestReconcile_ListErrorIsTransientNotDegradation(t *testing.T) {
	apl := &recordingApplier{}
	aud := &recordingAudit{}
	stat := &recordingStatus{}
	r := &firewall.Reconciler{
		Lister:       (&fakeLister{err: errors.New("nft exec failed")}).List,
		Applier:      apl.Apply,
		Audit:        aud.fn(),
		UpdateStatus: stat.fn(),
		Render:       goodInput,
	}
	err := r.Tick(context.Background())
	if err == nil {
		t.Fatalf("expected Tick to surface list error")
	}
	if len(stat.Updates()) != 0 {
		t.Errorf("status update fired on transient list error: %v", stat.Updates())
	}
}

// silence unused import warning
var _ = fmt.Sprintf
