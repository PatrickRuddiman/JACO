// Package stagefirst holds the pure decision logic for JACO's stage-first
// ACME issuance (issue #41): for a domain that is new to the cluster's cert
// state, issue against Let's Encrypt staging first, run a cheap self-check on
// the issued chain, and only flip to prod once staging succeeds. A
// DNS/firewall/routing misconfig burns a cheap staging failure instead of a
// prod rate-limit hit.
//
// This package is deliberately I/O-free: it answers "should we stage this
// domain?" and "does this staging chain look good?" so the embedded-mode
// daemon controller (the only mode that can programmatically re-issue +
// reload Caddy) can act on the decision. JACO_INGRESS_EXEC=1 does NOT get
// stage-first — an external caddy owns issuance there.
package stagefirst

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
)

// LE directory URLs. Mirrors internal/daemon/config so the decision logic
// can classify a configured CA without importing the daemon config package.
const (
	LEProdCA    = "https://acme-v02.api.letsencrypt.org/directory"
	LEStagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// IsProdCA reports whether ca is Let's Encrypt production. An empty CA means
// "default", which resolves to prod. Any other (staging, ZeroSSL, Pebble,
// a pinned dev CA) is treated as non-prod, so stage-first is skipped — the
// operator already opted into a non-prod directory.
func IsProdCA(ca string) bool {
	c := strings.TrimSpace(ca)
	return c == "" || c == LEProdCA
}

// Decision is the output of ShouldStage: whether to run the staging dry-run
// first and which directory URLs the two stages target.
type Decision struct {
	// Stage is true when the daemon should issue against StagingCA first.
	Stage bool
	// StagingCA is the directory URL for the dry run (LE staging) when Stage.
	StagingCA string
	// ProdCA is the directory the real cert is issued against (the operator's
	// configured CA — prod by default).
	ProdCA string
	// Reason is a human-readable why, for structured logs/audit.
	Reason string
}

// Params feeds the stage-first decision.
type Params struct {
	// Domain is the SAN being issued.
	Domain string
	// ConfiguredCA is jacod.yaml.acme_ca (empty → LE prod).
	ConfiguredCA string
	// SkipStaging is jacod.yaml.acme_skip_staging.
	SkipStaging bool
	// AlreadyIssued reports whether the cluster's cert state already holds a
	// (prod) cert for this domain — if so there's nothing new to stage.
	AlreadyIssued bool
}

// ShouldStage decides whether a new domain should be issued against staging
// before prod. Order of precedence:
//
//  1. Configured CA already non-prod  → no stage (operator pinned staging/dev).
//  2. acme_skip_staging: true         → no stage (operator opted out).
//  3. Domain already has a cert        → no stage (not new).
//  4. Otherwise                        → stage first.
func ShouldStage(p Params) Decision {
	prod := p.ConfiguredCA
	if prod == "" {
		prod = LEProdCA
	}
	switch {
	case !IsProdCA(p.ConfiguredCA):
		return Decision{Stage: false, ProdCA: prod, Reason: "configured CA is already non-prod; staging skipped"}
	case p.SkipStaging:
		return Decision{Stage: false, ProdCA: prod, Reason: "acme_skip_staging set; issuing directly against prod"}
	case p.AlreadyIssued:
		return Decision{Stage: false, ProdCA: prod, Reason: "domain already has a cert; no staging dry-run needed"}
	default:
		return Decision{Stage: true, StagingCA: LEStagingCA, ProdCA: prod, Reason: "new domain on prod CA; staging dry-run first"}
	}
}

// SelfCheck implements the cheap staging self-check (issue #41 Q2 option a):
// the issued chain parses as an X.509 cert and its SANs cover the expected
// domain. We deliberately do NOT do a full HTTPS handshake to ourselves
// (cyclic + expensive) nor validate against staging's intermediate (the
// staging root isn't in the system trust store). A parse + SAN match is
// sufficient to prove DNS/HTTP-01 validation succeeded and the CA handed
// back a usable leaf.
func SelfCheck(domain string, chainPEM []byte) error {
	if len(chainPEM) == 0 {
		return fmt.Errorf("stage self-check %q: empty cert chain", domain)
	}
	var leaf *x509.Certificate
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("stage self-check %q: parse cert: %w", domain, err)
		}
		// The first certificate in a chain is the leaf.
		leaf = c
		break
	}
	if leaf == nil {
		return fmt.Errorf("stage self-check %q: no CERTIFICATE block in chain", domain)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return fmt.Errorf("stage self-check %q: SAN mismatch: %w", domain, err)
	}
	return nil
}
