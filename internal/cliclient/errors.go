package cliclient

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// RenderError writes a human-readable representation of a typed pb.Error to w
// (typically os.Stderr). Format:
//
//	Error: <code> — <message>
//	  <key>=<value>
//	  <key>=<value>
//
// Details keys are emitted in alphabetical order so the output is stable
// across runs.
func RenderError(w io.Writer, e *pb.Error) {
	if e == nil {
		fmt.Fprintln(w, "Error: unknown")
		return
	}
	fmt.Fprintf(w, "Error: %s — %s\n", e.GetCode(), e.GetMessage())
	if d := e.GetDetails(); len(d) > 0 {
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "  %s=%s\n", k, d[k])
		}
	}
}

// RenderConnectionError writes the standard "Connection error: <addr>: <reason>"
// line used by the CLI when transport-level failures rotate past every
// configured endpoint.
func RenderConnectionError(w io.Writer, addr, reason string) {
	fmt.Fprintf(w, "Connection error: %s: %s\n", addr, reason)
}

// ExtractError attempts to decode a typed pb.Error from a gRPC status error's
// details. Returns nil when err is not a status or carries no Error proto;
// callers fall back to err.Error() in that case.
func ExtractError(err error) *pb.Error {
	if err == nil {
		return nil
	}
	sErr, ok := status.FromError(err)
	if !ok {
		return nil
	}
	for _, d := range sErr.Details() {
		if e, ok := d.(*pb.Error); ok {
			return e
		}
	}
	// No typed detail; synthesize one from the status message + code.
	return &pb.Error{
		Code:    sErr.Code().String(),
		Message: sErr.Message(),
	}
}

// UnpackStatus returns the pb.Error code and message attached to a gRPC status
// when a pb.Error detail is present. Returns ok=false when err is nil, not a
// gRPC status, or carries no pb.Error detail. Unlike ExtractError it does not
// synthesize a fallback from the raw status fields.
func UnpackStatus(err error) (code, message string, ok bool) {
	if err == nil {
		return "", "", false
	}
	st, isGRPC := status.FromError(err)
	if !isGRPC {
		return "", "", false
	}
	for _, d := range st.Details() {
		if e, cast := d.(*pb.Error); cast {
			return e.GetCode(), e.GetMessage(), true
		}
	}
	return "", "", false
}

// FormatError returns a human-readable error string from a gRPC error. When
// the error carries a pb.Error detail the format is "Error: <code>: <message>".
// When no pb.Error detail is present it falls back to err.Error(). Returns nil
// when err is nil.
func FormatError(err error) error {
	if err == nil {
		return nil
	}
	if code, message, ok := UnpackStatus(err); ok {
		return fmt.Errorf("Error: %s: %s", code, message)
	}
	return err
}

// errInvalidWriter is reserved for future helper that detects when the
// renderer is being asked to write to a non-Writer (currently unused).
var errInvalidWriter = errors.New("renderer requires a non-nil writer")

func init() { _ = errInvalidWriter }
