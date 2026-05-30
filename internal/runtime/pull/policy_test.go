package pull

import "testing"

// TestShouldPull — issue #120: every accepted pull_policy except `never`
// must keep the existing pull path active. The empty string (operator
// did not set pull_policy) must behave as the default — pull.
func TestShouldPull(t *testing.T) {
	for _, tc := range []struct {
		policy Policy
		want   bool
	}{
		{PolicyDefault, true},
		{PolicyAlways, true},
		{PolicyMissing, true},
		{PolicyBuild, true},
		{PolicyNever, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			if got := ShouldPull(tc.policy); got != tc.want {
				t.Errorf("ShouldPull(%q) = %v, want %v", tc.policy, got, tc.want)
			}
		})
	}
}
