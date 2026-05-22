package volumes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"io"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/volumes"
)

// fakeDocker is the minimal partial implementation tests use. Methods that
// aren't exercised panic — anything missing is a wiring bug, not silently
// passing.
type fakeDocker struct {
	dockerx.Docker
	volumeCreateFn func(ctx context.Context, opts volume.CreateOptions) (volume.Volume, error)
}

func (f *fakeDocker) VolumeCreate(ctx context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	if f.volumeCreateFn == nil {
		panic("volumeCreateFn not set")
	}
	return f.volumeCreateFn(ctx, opts)
}

func TestEnsureNamedVolume_CallsDockerWithName(t *testing.T) {
	var got string
	d := &fakeDocker{
		volumeCreateFn: func(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
			got = opts.Name
			return volume.Volume{Name: opts.Name}, nil
		},
	}
	if err := volumes.EnsureNamedVolume(context.Background(), d, "logs"); err != nil {
		t.Fatalf("EnsureNamedVolume: %v", err)
	}
	if got != "logs" {
		t.Errorf("VolumeCreate Name = %q, want logs", got)
	}
}

func TestEnsureNamedVolume_EmptyNameRejected(t *testing.T) {
	d := &fakeDocker{}
	if err := volumes.EnsureNamedVolume(context.Background(), d, ""); err == nil {
		t.Fatalf("expected error on empty name")
	}
}

func TestEnsureNamedVolume_PropagatesDockerError(t *testing.T) {
	d := &fakeDocker{
		volumeCreateFn: func(_ context.Context, _ volume.CreateOptions) (volume.Volume, error) {
			return volume.Volume{}, errors.New("docker is down")
		},
	}
	err := volumes.EnsureNamedVolume(context.Background(), d, "x")
	if err == nil || !strings.Contains(err.Error(), "docker is down") {
		t.Errorf("err = %v; want wrap of 'docker is down'", err)
	}
}

func TestValidateBindMount_ExistingPath(t *testing.T) {
	dir := t.TempDir()
	if err := volumes.ValidateBindMount(dir); err != nil {
		t.Errorf("existing dir rejected: %v", err)
	}
	// A regular file is also a valid bind source.
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := volumes.ValidateBindMount(f); err != nil {
		t.Errorf("existing file rejected: %v", err)
	}
}

func TestValidateBindMount_MissingPathReturnsTypedError(t *testing.T) {
	err := volumes.ValidateBindMount(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !volumes.IsBindMountInvalid(err) {
		t.Errorf("err is not bind_mount_invalid: %v", err)
	}
	var ve *volumes.Error
	if !errors.As(err, &ve) {
		t.Fatalf("err is not *volumes.Error: %T", err)
	}
	if ve.Code != "bind_mount_invalid" {
		t.Errorf("code = %q, want bind_mount_invalid", ve.Code)
	}
}

func TestValidateBindMount_EmptySourceRejected(t *testing.T) {
	err := volumes.ValidateBindMount("")
	if !volumes.IsBindMountInvalid(err) {
		t.Errorf("empty source should be bind_mount_invalid: %v", err)
	}
}

// silence import-must-be-used checks for the docker types we keep on hand
// so the fakeDocker partial-impl method signatures compile.
var (
	_ = types.ContainerJSON{}
	_ = container.Config{}
	_ = image.PullOptions{}
	_ = network.NetworkingConfig{}
	_ = ocispec.Platform{}
	_ io.Reader
)
