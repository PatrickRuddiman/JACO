package cliclient

import (
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

// ExtractError decodes a typed pb.Error from a gRPC status error's details.
// When the error is a gRPC status but carries no pb.Error detail, a
// synthetic pb.Error is constructed from the status code and message so
// callers always have something renderable for any non-nil status error.
//
// Returns nil only when err is nil or when err is not a gRPC status error
// (for example, a plain error returned from local code). For any non-nil
// gRPC status error this always returns a non-nil *pb.Error.
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

// formattedError carries a CLI-facing message while preserving the original
// underlying error so callers can still use errors.As / errors.Is /
// errors.Unwrap to recover the gRPC status (or any other typed cause).
type formattedError struct {
	msg   string
	cause error
}

// Error returns only the operator-facing message; the underlying cause's
// text is intentionally omitted to keep stderr output clean.
func (e *formattedError) Error() string { return e.msg }

// Unwrap exposes the original error so errors.As(*status.Status) and
// similar checks keep working after FormatError has run.
func (e *formattedError) Unwrap() error { return e.cause }

// FormatError returns a human-readable error wrapper for a gRPC error. When
// the error carries a pb.Error detail the displayed message is
// "Error: <code>: <message>". When no pb.Error detail is present, the
// returned error's message matches err.Error(). In both cases the original
// error remains reachable via errors.Unwrap / errors.As, so callers can
// still recover the gRPC *status.Status. Returns nil when err is nil.
func FormatError(err error) error {
	if err == nil {
		return nil
	}
	if code, message, ok := UnpackStatus(err); ok {
		return &formattedError{
			msg:   fmt.Sprintf("Error: %s: %s", code, message),
			cause: err,
		}
	}
	return err
}
