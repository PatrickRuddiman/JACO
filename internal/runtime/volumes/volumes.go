// Package volumes provides idempotent named-volume creation and bind-mount
// preflight checks for the runtime slice. Both helpers take the narrow
// dockerx.Docker interface so they can be unit-tested with a fake.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/docker/docker/api/types/volume"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// Error is the typed result returned by ValidateBindMount when the operator
// supplied a path that JACO can't safely mount. Code is "bind_mount_invalid".
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
