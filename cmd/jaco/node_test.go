package main

import (
	"strings"
	"testing"
	"time"
)

func TestFormatIssueJoinToken_DefaultOutput(t *testing.T) {
	got := formatIssueJoinToken("jaco-1:7000", "tok-abc123", 24*time.Hour, "CERT_PEM", false)

	wantContains := []string{
		"Join token issued.",
		"sudo jaco node join --peer=jaco-1:7000 --token=tok-abc123",
		"Token expires in 24h (single-use).",
	}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}

	// CA must not appear in default output
	if strings.Contains(got, "CERT_PEM") {
		t.Errorf("CA cert unexpectedly present in default output:\n%s", got)
	}
}

func TestFormatIssueJoinToken_ShowCA(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nABCDEF\n-----END CERTIFICATE-----\n"
	got := formatIssueJoinToken("jaco-1:7000", "tok-xyz", 24*time.Hour, ca, true)

	if !strings.Contains(got, "Cluster CA") {
		t.Errorf("expected 'Cluster CA' section when showCA=true:\n%s", got)
	}
	if !strings.Contains(got, ca) {
		t.Errorf("expected CA PEM in output when showCA=true:\n%s", got)
	}
}

func TestFormatIssueJoinToken_ShowCA_EmptyCert(t *testing.T) {
	// When the server returns an empty CA, --show-ca should not add noise
	got := formatIssueJoinToken("jaco-1:7000", "tok-xyz", 24*time.Hour, "", true)

	if strings.Contains(got, "Cluster CA") {
		t.Errorf("unexpected 'Cluster CA' section for empty cert:\n%s", got)
	}
}

func TestFormatIssueJoinToken_DifferentExpiry(t *testing.T) {
	got := formatIssueJoinToken("srv:9000", "tok-exp", 1*time.Hour, "", false)

	if !strings.Contains(got, "1h") {
		t.Errorf("expected expiry '1h' in output:\n%s", got)
	}
}

func TestFormatIssueJoinToken_FractionalExpiry(t *testing.T) {
	// 90 minutes is not a whole number of hours; falls back to Duration.String()
	got := formatIssueJoinToken("srv:9000", "tok-exp", 90*time.Minute, "", false)

	if !strings.Contains(got, "1h30m0s") {
		t.Errorf("expected expiry '1h30m0s' in output:\n%s", got)
	}
}

func TestFormatIssueJoinToken_IncludesServerAndToken(t *testing.T) {
	server := "my-cluster.example.com:7000"
	token := "supersecrettoken"
	got := formatIssueJoinToken(server, token, 24*time.Hour, "", false)

	if !strings.Contains(got, "--peer="+server) {
		t.Errorf("output missing --peer=%s:\n%s", server, got)
	}
	if !strings.Contains(got, "--token="+token) {
		t.Errorf("output missing --token=%s:\n%s", token, got)
	}
}
