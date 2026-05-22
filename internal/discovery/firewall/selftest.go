package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
)

// SelfTestError is the typed result SelfTest returns when the host's
// nftables state diverges from what JACO rendered. Code is
// "isolation_self_test_failed"; details surfaces what's missing / extra so
// the operator can debug.
type SelfTestError struct {
	Code    string
	Message string
	Missing []string
	Extra   []string
}

// Error implements the error interface.
func (e *SelfTestError) Error() string { return e.Message }

// SelfTest runs `nft -j list table inet jaco` and asserts that the live
// ruleset matches the expected shape: chains forward / input / output with
// the right hook + policy, plus a set per expected (deployment, network).
//
// Returns *SelfTestError on mismatch.
func SelfTest(ctx context.Context, expected RuleInput) error {
	cmd := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "jaco")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("nft -j list: %w", err)
	}
	return SelfTestFromJSON(out, expected)
}

// SelfTestFromJSON is the JSON-parsing core that production calls indirect
// through SelfTest; exposed so unit tests can supply a canned nft output.
func SelfTestFromJSON(jsonBytes []byte, expected RuleInput) error {
	var doc nftablesDoc
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return fmt.Errorf("parse nft json: %w", err)
	}

	gotChains := map[string]*nftChain{}
	gotSets := map[string]bool{}
	for _, e := range doc.Nftables {
		if e.Chain != nil {
			c := *e.Chain
			gotChains[c.Name] = &c
		}
		if e.Set != nil {
			gotSets[e.Set.Name] = true
		}
	}

	wantChains := map[string]struct {
		hook, policy string
		priority     int
	}{
		"forward": {hook: "forward", policy: "drop", priority: 0},
		"input":   {hook: "input", policy: "drop", priority: 0},
		"output":  {hook: "output", policy: "accept", priority: 0},
	}

	var missing, extra []string
	for name, want := range wantChains {
		got, ok := gotChains[name]
		if !ok {
			missing = append(missing, "chain:"+name)
			continue
		}
		if got.Hook != want.hook {
			missing = append(missing, fmt.Sprintf("chain:%s.hook=%s (want %s)", name, got.Hook, want.hook))
		}
		if got.Policy != "" && got.Policy != want.policy {
			missing = append(missing, fmt.Sprintf("chain:%s.policy=%s (want %s)", name, got.Policy, want.policy))
		}
		if got.Priority != want.priority {
			missing = append(missing, fmt.Sprintf("chain:%s.priority=%d (want %d)", name, got.Priority, want.priority))
		}
	}

	wantSets := map[string]bool{}
	for _, s := range expected.Subnets {
		wantSets[SetName(s.Deployment, s.Network)] = true
	}
	for name := range wantSets {
		if !gotSets[name] {
			missing = append(missing, "set:"+name)
		}
	}
	for name := range gotSets {
		if !wantSets[name] {
			extra = append(extra, "set:"+name)
		}
	}

	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return &SelfTestError{
			Code:    "isolation_self_test_failed",
			Message: fmt.Sprintf("nftables state diverges: missing=%v extra=%v", missing, extra),
			Missing: missing,
			Extra:   extra,
		}
	}
	return nil
}

// --- nft -j json shape -----------------------------------------------------

type nftablesDoc struct {
	Nftables []nftEntry `json:"nftables"`
}

type nftEntry struct {
	Chain *nftChain `json:"chain,omitempty"`
	Set   *nftSet   `json:"set,omitempty"`
}

type nftChain struct {
	Family   string `json:"family"`
	Table    string `json:"table"`
	Name     string `json:"name"`
	Hook     string `json:"hook"`
	Priority int    `json:"prio"`
	Policy   string `json:"policy"`
}

type nftSet struct {
	Family string `json:"family"`
	Name   string `json:"name"`
	Table  string `json:"table"`
	Type   string `json:"type"`
}
