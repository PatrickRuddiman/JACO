package cliclient_test

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// --- UnpackStatus ------------------------------------------------------------

func TestUnpackStatus_NilError_ReturnsFalse(t *testing.T) {
	_, _, ok := cliclient.UnpackStatus(nil)
	if ok {
		t.Error("UnpackStatus(nil): ok=true, want false")
	}
}

func TestUnpackStatus_PlainError_ReturnsFalse(t *testing.T) {
	_, _, ok := cliclient.UnpackStatus(errors.New("some plain error"))
	if ok {
		t.Error("UnpackStatus(plain error): ok=true, want false")
	}
}

func TestUnpackStatus_GrpcStatusNoDetails_ReturnsFalse(t *testing.T) {
	err := status.Error(codes.InvalidArgument, "validation_failed")
	_, _, ok := cliclient.UnpackStatus(err)
	if ok {
		t.Error("UnpackStatus(gRPC status with no pb.Error detail): ok=true, want false")
	}
}

func TestUnpackStatus_GrpcStatusWithPbErrorDetail_ReturnsCodeAndMessage(t *testing.T) {
	st, err := status.New(codes.InvalidArgument, "validation_failed").
		WithDetails(&pb.Error{
			Code:    "VALIDATION_FAILED",
			Message: `service "api" uses unsupported field "build"`,
		})
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}

	code, message, ok := cliclient.UnpackStatus(st.Err())
	if !ok {
		t.Fatal("UnpackStatus: ok=false, want true")
	}
	if code != "VALIDATION_FAILED" {
		t.Errorf("code = %q, want %q", code, "VALIDATION_FAILED")
	}
	want := `service "api" uses unsupported field "build"`
	if message != want {
		t.Errorf("message = %q, want %q", message, want)
	}
}

// --- FormatError -------------------------------------------------------------

func TestFormatError_NilError_ReturnsNil(t *testing.T) {
	if got := cliclient.FormatError(nil); got != nil {
		t.Errorf("FormatError(nil) = %v, want nil", got)
	}
}

func TestFormatError_PlainError_PassesThrough(t *testing.T) {
	orig := errors.New("plain error")
	got := cliclient.FormatError(orig)
	if got != orig {
		t.Errorf("FormatError(plain) = %v, want unchanged error", got)
	}
}

func TestFormatError_GrpcStatusNoDetails_FallsBackToErrError(t *testing.T) {
	err := status.Error(codes.InvalidArgument, "validation_failed")
	got := cliclient.FormatError(err)
	if got == nil {
		t.Fatal("FormatError returned nil for non-nil error")
	}
	if got.Error() != err.Error() {
		t.Errorf("FormatError (no details) = %q, want original %q", got.Error(), err.Error())
	}
}

func TestFormatError_GrpcStatusWithPbErrorDetail_RendersHumanReadable(t *testing.T) {
	st, err := status.New(codes.InvalidArgument, "validation_failed").
		WithDetails(&pb.Error{
			Code:    "VALIDATION_FAILED",
			Message: `service "api" uses unsupported field "build"`,
		})
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}

	got := cliclient.FormatError(st.Err())
	if got == nil {
		t.Fatal("FormatError returned nil for non-nil error")
	}
	want := `Error: VALIDATION_FAILED: service "api" uses unsupported field "build"`
	if got.Error() != want {
		t.Errorf("FormatError = %q, want %q", got.Error(), want)
	}
	// Must not contain the raw gRPC prefix.
	if strings.Contains(got.Error(), "rpc error") {
		t.Errorf("FormatError output contains raw rpc error prefix: %q", got.Error())
	}
}

func TestFormatError_GrpcStatusWithPbErrorDetail_PreservesUnderlyingStatus(t *testing.T) {
	st, err := status.New(codes.InvalidArgument, "validation_failed").
		WithDetails(&pb.Error{
			Code:    "VALIDATION_FAILED",
			Message: "bad",
		})
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	orig := st.Err()

	got := cliclient.FormatError(orig)
	if got == nil {
		t.Fatal("FormatError returned nil for non-nil error")
	}

	// errors.Unwrap should return the original gRPC status error so callers
	// can still inspect codes, details, etc.
	if unwrapped := errors.Unwrap(got); unwrapped != orig {
		t.Errorf("errors.Unwrap(FormatError(...)) = %v, want original gRPC error", unwrapped)
	}

	// status.FromError walks Unwrap chains via the GRPCStatus interface, so
	// the wrapped error should still be recognized as a gRPC status with
	// the same code as the original.
	rec, ok := status.FromError(got)
	if !ok {
		t.Fatal("status.FromError(FormatError(...)): ok=false, want true")
	}
	if rec.Code() != codes.InvalidArgument {
		t.Errorf("recovered code = %v, want %v", rec.Code(), codes.InvalidArgument)
	}
}
