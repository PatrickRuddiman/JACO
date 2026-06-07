package grpcsrv

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Status returns a snapshot of the (filtered) deployment(s), their observed
// replicas, and their routes. Reads from local state; no leader required so
// the CLI can hit any node.
func (d *deployServer) Status(_ context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
	depFilter := req.GetDeploymentFilter()
	svcFilter := req.GetServiceFilter()

	resp := &pb.DeployStatusResponse{}

	for _, dep := range d.state.Deployments.List() {
		if depFilter != "" && dep.GetName() != depFilter {
			continue
		}
		resp.Deployments = append(resp.Deployments, dep)
	}

	// Replica filtering needs the per-replica deployment/service which lives
	// on ReplicaDesired (ReplicaObserved doesn't carry it). Index desired by
	// id so the per-observed lookup stays O(1).
	desiredByID := map[string]*pb.ReplicaDesired{}
	for _, r := range d.state.ReplicasDesired.List() {
		desiredByID[r.GetId()] = r
	}
	for _, r := range d.state.ReplicasObserved.List() {
		if depFilter != "" || svcFilter != "" {
			rd, ok := desiredByID[r.GetId()]
			if !ok {
				continue
			}
			if depFilter != "" && rd.GetDeployment() != depFilter {
				continue
			}
			if svcFilter != "" && rd.GetService() != svcFilter {
				continue
			}
		}
		resp.Replicas = append(resp.Replicas, r)
	}

	// Track the in-scope domains so cert state is reported only for routes the
	// caller asked about.
	domains := map[string]bool{}
	for _, rt := range d.state.Routes.List() {
		if depFilter != "" && rt.GetDeployment() != depFilter {
			continue
		}
		if svcFilter != "" && rt.GetService() != svcFilter {
			continue
		}
		resp.Routes = append(resp.Routes, rt)
		if rt.GetTlsAuto() {
			domains[rt.GetDomain()] = true
		}
	}

	// Per-domain cert state for the in-scope domains: not_after from the
	// raft-replicated leaf cert blob, environment + last_renewal_at from the
	// most recent cert audit event for that domain (issue #41). Domains with
	// no observable cert yet are omitted.
	if len(domains) > 0 {
		// Latest cert audit event per domain → environment + timestamp.
		type auditInfo struct {
			env string
			ts  *timestamppb.Timestamp
		}
		latest := map[string]auditInfo{}
		for _, ev := range d.state.AuditEvents.List() {
			switch ev.GetType() {
			case pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED,
				pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED:
			default:
				continue
			}
			dom := ev.GetPayload()["domain"]
			if dom == "" || !domains[dom] {
				continue
			}
			// AuditEvents.List() returns append order (raft index ascending),
			// so the last matching event wins.
			latest[dom] = auditInfo{
				env: ev.GetPayload()["acme_environment"],
				ts:  ev.GetTs(),
			}
		}

		// leafNotAfter parses the first CERTIFICATE block out of a PEM chain
		// and returns its NotAfter as a proto timestamp, or nil when the blob
		// isn't a parseable cert.
		leafNotAfter := func(pemBytes []byte) *timestamppb.Timestamp {
			rest := pemBytes
			for {
				block, r := pem.Decode(rest)
				if block == nil {
					return nil
				}
				rest = r
				if block.Type != "CERTIFICATE" {
					continue
				}
				c, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil
				}
				return timestamppb.New(c.NotAfter)
			}
		}

		// not_after AND env from the leaf cert blob. certmagic stores the
		// leaf chain at a key ending in "/<domain>.crt"; the value is PEM.
		// The key also contains the issuing CA host ("acme-staging-v02..."
		// for LE staging, "acme-v02..." for prod), so it doubles as the
		// authoritative environment signal — independent of whether an
		// ISSUED audit event has fired yet. Closes the window in #147
		// where a prod cert lands in raft but the controller hasn't yet
		// ticked to emit CERTIFICATE_ISSUED(prod): status reports `prod`
		// the moment the blob exists. The audit event still wins for the
		// last_renewal_at timestamp.
		type certInfo struct {
			notAfter *timestamppb.Timestamp
			env      string
		}
		fromBlob := map[string]certInfo{}
		for _, b := range d.state.CertBlobs.List() {
			key := b.GetKey()
			if !strings.HasSuffix(key, ".crt") {
				continue
			}
			for dom := range domains {
				if !strings.Contains(key, "/"+dom+"/") {
					continue
				}
				t := leafNotAfter(b.GetValue())
				if t == nil {
					continue
				}
				env := envFromCertKey(key)
				// Prefer the prod blob when both staging and prod blobs
				// exist for the same domain (mid-promotion window).
				if prev, ok := fromBlob[dom]; ok && prev.env == "prod" && env != "prod" {
					continue
				}
				fromBlob[dom] = certInfo{notAfter: t, env: env}
			}
		}

		for dom := range domains {
			info, hasAudit := latest[dom]
			blob, hasCert := fromBlob[dom]
			if !hasAudit && !hasCert {
				continue
			}
			cs := &pb.CertState{Domain: dom}
			if hasCert {
				cs.NotAfter = blob.notAfter
				cs.Environment = blob.env
			}
			if hasAudit {
				// Audit drives last_renewal_at always; environment only
				// when the blob couldn't classify (e.g., an issued event
				// landed before the blob did — unusual but possible
				// across replication).
				cs.LastRenewalAt = info.ts
				if cs.Environment == "" {
					cs.Environment = info.env
				}
			}
			resp.Certs = append(resp.Certs, cs)
		}
	}
	return resp, nil
}

// envFromCertKey classifies a certmagic blob key as "staging" or "prod"
// by the issuing CA host embedded in the key
// ("certificates/<ca-host>/<domain>/..."). Staging is the only LE
// directory whose host substring is `staging`; everything else (LE prod,
// ZeroSSL, custom prod ACMEs) is reported as `prod`. Matches the
// loadStagingChain / prodCertIssued heuristics in
// internal/daemon/grpc/ingress.go so status and the controller agree on
// which blobs are staging.
func envFromCertKey(key string) string {
	if strings.Contains(key, "staging") {
		return "staging"
	}
	return "prod"
}
