package cliclient_test

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func grpcStatusErr(msg string) error {
	return status.Error(codes.Unauthenticated, msg)
}
