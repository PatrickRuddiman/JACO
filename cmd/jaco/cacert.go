package main

import "os"

// defaultCACertPath returns JACO_CA_CERT if set, otherwise the
// install-time location at /var/lib/jaco/node/ca.crt.
func defaultCACertPath() string {
	if v := os.Getenv("JACO_CA_CERT"); v != "" {
		return v
	}
	return "/var/lib/jaco/node/ca.crt"
}
