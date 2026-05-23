package grpcsrv_test

import (
	"testing"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

// TestFactories_ReturnNonNil — the daemon's startSubsystems uses these
// to wire the proxy targets. They're thin constructors; assert each
// produces a non-nil interface so the wiring stays plumbable.
func TestFactories_ReturnNonNil(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)

	if grpcsrv.NewClusterServer(st, nil) == nil {
		t.Errorf("NewClusterServer = nil")
	}
	if grpcsrv.NewTokensServer(st, nil) == nil {
		t.Errorf("NewTokensServer = nil")
	}
	if grpcsrv.NewDeployServer(st, nil) == nil {
		t.Errorf("NewDeployServer = nil")
	}
	if grpcsrv.NewAuditServer(st, brokers) == nil {
		t.Errorf("NewAuditServer = nil")
	}
	if grpcsrv.NewWatchServer(st, brokers) == nil {
		t.Errorf("NewWatchServer = nil")
	}
}
