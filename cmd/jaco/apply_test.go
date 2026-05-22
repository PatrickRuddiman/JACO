package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeDeployClient implements pb.DeployClient for unit tests.
type fakeDeployClient struct {
	pb.DeployClient
	applyFn    func(ctx context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error)
	rollbackFn func(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error)
	deleteFn   func(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error)
}

func (f *fakeDeployClient) Apply(ctx context.Context, req *pb.ApplyRequest, _ ...grpc.CallOption) (*pb.ApplyResponse, error) {
	return f.applyFn(ctx, req)
}
func (f *fakeDeployClient) Rollback(ctx context.Context, req *pb.RollbackRequest, _ ...grpc.CallOption) (*pb.RollbackResponse, error) {
	return f.rollbackFn(ctx, req)
}
func (f *fakeDeployClient) Delete(ctx context.Context, req *pb.DeleteRequest, _ ...grpc.CallOption) (*pb.DeleteResponse, error) {
	return f.deleteFn(ctx, req)
}

func TestApply_DryRunPrintsNoChangesOnEmptyDiff(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		applyFn: func(_ context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
			if !req.GetDryRun() {
				t.Errorf("expected dry_run=true; got false")
			}
			return &pb.ApplyResponse{Diff: &pb.Diff{}}, nil
		},
	}
	if err := runApply(context.Background(), client, []byte("y"), []byte("c"), true, &out); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if !strings.Contains(out.String(), "No changes") {
		t.Errorf("expected 'No changes' on empty diff; got %q", out.String())
	}
}

func TestApply_DryRunPrintsPopulatedDiff(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		applyFn: func(_ context.Context, _ *pb.ApplyRequest) (*pb.ApplyResponse, error) {
			return &pb.ApplyResponse{Diff: &pb.Diff{
				Adds:    []string{"replica sample-web-0", "route web.example.com"},
				Updates: []string{"deployment sample (rev 1→2)"},
				Removes: []string{"route old.example.com"},
			}}, nil
		},
	}
	if err := runApply(context.Background(), client, []byte("y"), []byte("c"), true, &out); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	got := out.String()
	for _, want := range []string{"+ replica sample-web-0", "+ route web.example.com",
		"~ deployment sample (rev 1→2)", "- route old.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestApply_PrintsAppliedRevisionOnSuccess(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		applyFn: func(_ context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
			if req.GetDryRun() {
				t.Errorf("expected dry_run=false; got true")
			}
			return &pb.ApplyResponse{AppliedRevision: 3}, nil
		},
	}
	if err := runApply(context.Background(), client, []byte("y"), []byte("c"), false, &out); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if !strings.Contains(out.String(), "Applied revision: 3") {
		t.Errorf("expected 'Applied revision: 3'; got %q", out.String())
	}
}

func TestApply_SurfacesServerError(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		applyFn: func(_ context.Context, _ *pb.ApplyRequest) (*pb.ApplyResponse, error) {
			return nil, errors.New("validation_failed: deployment is required")
		},
	}
	err := runApply(context.Background(), client, []byte{}, []byte{}, false, &out)
	if err == nil {
		t.Fatalf("expected error to bubble up")
	}
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Errorf("err = %v", err)
	}
}

func TestRollback_PrintsNewRevision(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		rollbackFn: func(_ context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
			if req.GetDeployment() != "sample" {
				t.Errorf("deployment = %q, want sample", req.GetDeployment())
			}
			return &pb.RollbackResponse{Revision: 1}, nil
		},
	}
	if err := runRollback(context.Background(), client, "sample", &out); err != nil {
		t.Fatalf("runRollback: %v", err)
	}
	if !strings.Contains(out.String(), "Rolled back to revision: 1") {
		t.Errorf("output = %q", out.String())
	}
}

func TestDelete_PrintsDeleted(t *testing.T) {
	var out bytes.Buffer
	client := &fakeDeployClient{
		deleteFn: func(_ context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
			if req.GetDeployment() != "sample" {
				t.Errorf("deployment = %q, want sample", req.GetDeployment())
			}
			return &pb.DeleteResponse{}, nil
		},
	}
	if err := runDelete(context.Background(), client, "sample", &out); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted deployment: sample") {
		t.Errorf("output = %q", out.String())
	}
}

func TestReadManifestPair_AutoDiscoversComposeNextToJacoYaml(t *testing.T) {
	dir := t.TempDir()
	jacoPath := filepath.Join(dir, "deploy.yml")
	composePath := filepath.Join(dir, "compose.yml")
	if err := writeFile(jacoPath, "deployment: x\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(composePath, "services:\n  web: { image: nginx }\n"); err != nil {
		t.Fatal(err)
	}
	jacoBytes, composeBytes, err := readManifestPair(jacoPath, "")
	if err != nil {
		t.Fatalf("readManifestPair: %v", err)
	}
	if !strings.Contains(string(jacoBytes), "deployment: x") {
		t.Errorf("jaco bytes wrong: %s", jacoBytes)
	}
	if !strings.Contains(string(composeBytes), "image: nginx") {
		t.Errorf("compose bytes wrong: %s", composeBytes)
	}
}

func TestReadManifestPair_ExplicitOverrideTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	jacoPath := filepath.Join(dir, "deploy.yml")
	composePath := filepath.Join(dir, "alt.yml")
	if err := writeFile(jacoPath, "deployment: x\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(composePath, "alt-marker\n"); err != nil {
		t.Fatal(err)
	}
	_, composeBytes, err := readManifestPair(jacoPath, composePath)
	if err != nil {
		t.Fatalf("readManifestPair: %v", err)
	}
	if !strings.Contains(string(composeBytes), "alt-marker") {
		t.Errorf("override not used: %s", composeBytes)
	}
}

func TestReadManifestPair_ErrorsWhenNoComposeFound(t *testing.T) {
	dir := t.TempDir()
	jacoPath := filepath.Join(dir, "deploy.yml")
	if err := writeFile(jacoPath, "deployment: x\n"); err != nil {
		t.Fatal(err)
	}
	_, _, err := readManifestPair(jacoPath, "")
	if err == nil {
		t.Fatalf("expected error when compose missing")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("err = %v", err)
	}
}

func TestApply_SampleFixturesAreValidPair(t *testing.T) {
	// Confirms the fixture files parse + are reachable via the auto-discovery.
	jacoBytes, composeBytes, err := readManifestPair(filepath.Join("testdata", "sample.jaco.yaml"), "")
	if err != nil {
		t.Fatalf("readManifestPair: %v", err)
	}
	if len(jacoBytes) == 0 || len(composeBytes) == 0 {
		t.Fatalf("empty fixture content")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
