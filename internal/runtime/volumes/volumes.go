// Package volumes validates managed-volume ownership and provides named-volume
// and bind-mount preflight helpers for the runtime.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// Error reports storage that JACO cannot safely mount.
type Error struct {
	Code    string
	Message string
	Path    string
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Message }

// IsBindMountInvalid reports whether err is a ValidateBindMount rejection.
func IsBindMountInvalid(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == "bind_mount_invalid"
	}
	return false
}

// EnsureNamedVolume idempotently calls VolumeCreate. Docker's API is itself
// idempotent under the same name + driver — re-creating a volume that
// already exists just returns the existing one — so this is essentially a
// pass-through with a typed error wrap.
func EnsureNamedVolume(ctx context.Context, d dockerx.Docker, name string) error {
	if name == "" {
		return fmt.Errorf("EnsureNamedVolume: empty name")
	}
	_, err := d.VolumeCreate(ctx, volume.CreateOptions{Name: name})
	if err != nil {
		return fmt.Errorf("volume create %s: %w", name, err)
	}
	return nil
}

// EnsureMounts validates storage before any container stop, start or replacement.
// existingID is empty when this replica has no container yet.
func EnsureMounts(ctx context.Context, d dockerx.Docker, spec compose.ContainerSpec, existingID string) error {
	mounted, err := checkExistingMounts(ctx, d, spec, existingID)
	if err != nil {
		return err
	}
	var pending []volume.CreateOptions
	for _, m := range spec.Mounts {
		if m.External {
			if m.Type != "volume" || m.Source == "" || m.DefaultVolumeKey != "" {
				return &Error{Code: "volume_identity_conflict", Path: m.Source, Message: "invalid external volume identity"}
			}
			v, err := d.VolumeInspect(ctx, m.Source)
			if errdefs.IsNotFound(err) {
				return &Error{
					Code: "external_volume_missing", Path: m.Source,
					Message: fmt.Sprintf("external volume %q does not exist on this Docker engine; refusing to create empty storage", m.Source),
				}
			}
			if err != nil {
				return fmt.Errorf("external volume inspect %s: %w", m.Source, err)
			}
			if v.Name != m.Source {
				return &Error{Code: "volume_identity_conflict", Path: m.Source, Message: fmt.Sprintf("external volume inspect %q returned a different volume %q", m.Source, v.Name)}
			}
			continue
		}
		if m.DefaultVolumeKey == "" {
			continue
		}
		if m.Type != "volume" || spec.ClusterID == "" || spec.Deployment == "" ||
			m.Source != compose.DefaultVolumeName(spec.ClusterID, spec.Deployment, m.DefaultVolumeKey) {
			return &Error{
				Code: "volume_identity_conflict", Path: m.Source,
				Message: fmt.Sprintf("volume %q has an incomplete or inconsistent managed identity; refusing to mount it", m.Source),
			}
		}
		opts := volume.CreateOptions{
			Name: m.Source,
			Labels: map[string]string{
				"jaco.volume_identity": "2",
				"jaco.cluster_id":      spec.ClusterID,
				"jaco.deployment":      spec.Deployment,
				"jaco.volume_key":      m.DefaultVolumeKey,
			},
		}
		v, err := d.VolumeInspect(ctx, m.Source)
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("volume inspect %s: %w", m.Source, err)
		}
		if errdefs.IsNotFound(err) {
			if mounted[m.Source] {
				return &Error{
					Code: "volume_migration_required", Path: m.Source,
					Message: fmt.Sprintf("container %q references missing volume %q; refusing to recreate empty storage. Recover and verify the original data before retrying", existingID, m.Source),
				}
			}
			legacy := "jaco_" + spec.Deployment + "_" + m.DefaultVolumeKey
			_, err := d.VolumeInspect(ctx, legacy)
			if err == nil {
				return &Error{
					Code: "volume_migration_required", Path: legacy,
					Message: fmt.Sprintf("legacy volume %q may contain data for deployment %q key %q; refusing an empty replacement. Verify its data and all consumers, then explicitly set name: %s and external: true to adopt it",
						legacy, spec.Deployment, m.DefaultVolumeKey, legacy),
				}
			}
			if !errdefs.IsNotFound(err) {
				return fmt.Errorf("legacy volume inspect %s: %w", legacy, err)
			}
			pending = append(pending, opts)
			continue
		}
		if err := checkOwnership(v, opts); err != nil {
			return err
		}
	}
	for _, opts := range pending {
		v, err := d.VolumeCreate(ctx, opts)
		if err != nil {
			return fmt.Errorf("volume create %s: %w", opts.Name, err)
		}
		// Create can return a volume created concurrently by another caller.
		if err := checkOwnership(v, opts); err != nil {
			return err
		}
	}
	return nil
}

func checkOwnership(v volume.Volume, opts volume.CreateOptions) error {
	owned := v.Name == opts.Name
	for key, want := range opts.Labels {
		owned = owned && v.Labels[key] == want
	}
	if !owned {
		return &Error{
			Code: "volume_identity_conflict", Path: opts.Name,
			Message: fmt.Sprintf("volume %q is not owned by cluster %q deployment %q key %q; refusing to mount it",
				opts.Name, opts.Labels["jaco.cluster_id"], opts.Labels["jaco.deployment"], opts.Labels["jaco.volume_key"]),
		}
	}
	return nil
}

func checkExistingMounts(ctx context.Context, d dockerx.Docker, spec compose.ContainerSpec, id string) (map[string]bool, error) {
	if id == "" {
		return nil, nil
	}
	var info *types.ContainerJSON
	var mounted map[string]bool
	for _, m := range spec.Mounts {
		if m.DefaultVolumeKey == "" {
			continue
		}
		if info == nil {
			current, err := d.ContainerInspect(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("inspect existing volume mounts for container %s: %w", id, err)
			}
			info = &current
		}
		for _, actual := range info.Mounts {
			if actual.Type == "volume" && actual.Name == m.Source {
				if mounted == nil {
					mounted = make(map[string]bool)
				}
				mounted[m.Source] = true
			}
			if actual.Destination == m.Target && (actual.Type != "volume" || actual.Name != m.Source) {
				return nil, &Error{
					Code: "volume_migration_required", Path: actual.Name,
					Message: fmt.Sprintf("container %q already mounts %q at %q instead of managed volume %q; leaving it untouched. Verify the existing data and consumers before adopting the intended volume with an explicit name and external: true",
						id, actual.Name, m.Target, m.Source),
				}
			}
		}
	}
	return mounted, nil
}

// ValidateBindMount checks that src exists and is readable. Returns a typed
// Error{Code:"bind_mount_invalid"} on failure so the runtime can surface it
// back through Deploy.Apply / ReplicaObserved without ambiguity.
func ValidateBindMount(src string) error {
	if src == "" {
		return &Error{Code: "bind_mount_invalid", Message: "bind mount source path is empty"}
	}
	info, err := os.Stat(src)
	if err != nil {
		return &Error{
			Code:    "bind_mount_invalid",
			Message: fmt.Sprintf("bind mount source %q is not accessible: %v", src, err),
			Path:    src,
		}
	}
	// We don't enforce IsDir — bind-mounting a single file is allowed by
	// docker — but we do check the path is at least readable by us.
	f, err := os.Open(src)
	if err != nil {
		return &Error{
			Code:    "bind_mount_invalid",
			Message: fmt.Sprintf("bind mount source %q is not readable: %v", src, err),
			Path:    src,
		}
	}
	_ = f.Close()
	_ = info
	return nil
}
