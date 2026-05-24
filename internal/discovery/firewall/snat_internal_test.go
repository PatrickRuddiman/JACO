package firewall

import (
	"reflect"
	"testing"
)

func TestSnatExemptRule(t *testing.T) {
	got := snatExemptRule("10.244.0.0/16")
	want := []string{"-s", "10.244.0.0/16", "-d", "10.244.0.0/16", "-j", "RETURN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snatExemptRule = %v, want %v", got, want)
	}
}
