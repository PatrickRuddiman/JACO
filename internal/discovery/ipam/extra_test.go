package ipam_test

import (
	"errors"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
)

// TestIPAMError_Format — exercises the Error() method.
func TestIPAMError_Format(t *testing.T) {
	err := &ipam.IPAMError{Code: "code-x", Message: "msg-y"}
	if err.Error() != "msg-y" {
		t.Errorf("Error() = %q, want msg-y", err.Error())
	}
}

// TestIsExhausted_OnlyMatchesPoolExhausted — IsExhausted returns true
// only for IPAMError{Code: "ipam_pool_exhausted"}; nil and other
// errors return false.
func TestIsExhausted_OnlyMatchesPoolExhausted(t *testing.T) {
	if ipam.IsExhausted(nil) {
		t.Errorf("IsExhausted(nil) = true")
	}
	if ipam.IsExhausted(errors.New("plain")) {
		t.Errorf("IsExhausted(plain) = true")
	}
	if ipam.IsExhausted(&ipam.IPAMError{Code: "other"}) {
		t.Errorf("IsExhausted(other code) = true")
	}
	if !ipam.IsExhausted(&ipam.IPAMError{Code: "ipam_pool_exhausted"}) {
		t.Errorf("IsExhausted(exhausted) = false")
	}
}

// TestNew_RejectsIPv6Pool — pool must be IPv4 /16.
func TestNew_RejectsIPv6Pool(t *testing.T) {
	if _, err := ipam.New(nil, nil, "2001:db8::/64"); err == nil {
		t.Errorf("New with IPv6 pool returned nil err")
	}
}

// TestNew_DefaultPoolWhenEmpty — empty poolCIDR uses DefaultPoolCIDR.
// (drives the "" branch in New.)
func TestNew_DefaultPoolWhenEmpty(t *testing.T) {
	if _, err := ipam.New(nil, func(_ []byte) error { return nil }, ""); err != nil {
		t.Errorf("New(empty) = %v, want nil (should use DefaultPoolCIDR)", err)
	}
}
