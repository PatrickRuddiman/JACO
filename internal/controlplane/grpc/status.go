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

		// not_after from the leaf cert blob. certmagic stores the leaf chain
		// at a key ending in "/<domain>.crt"; the value is PEM.
		notAfter := map[string]*timestamppb.Timestamp{}
		for _, b := range d.state.CertBlobs.List() {
			key := b.GetKey()
			if !strings.HasSuffix(key, ".crt") {
				continue
			}
			for dom := range domains {
				if !strings.Contains(key, "/"+dom+"/") {
					continue
				}
				if t := leafNotAfter(b.GetValue()); t != nil {
					notAfter[dom] = t
				}
			}
		}

		for dom := range domains {
			info, hasAudit := latest[dom]
			na, hasCert := notAfter[dom]
			if !hasAudit && !hasCert {
				continue
			}
			cs := &pb.CertState{Domain: dom, NotAfter: na}
			if hasAudit {
				cs.Environment = info.env
				cs.LastRenewalAt = info.ts
			}
			resp.Certs = append(resp.Certs, cs)
		}
	}
	return resp, nil
}
