//go:build !unix

package grpc

import "fmt"

func checkIngressAdminDir(path string) error {
	return fmt.Errorf("caddy admin directory %s requires Unix filesystem permissions", path)
}
