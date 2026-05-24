package firewall

import "testing"

func TestIsNftTableNotFound(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"absent table with hint", "Error: No such file or directory; did you mean table 'foo' in family ip?\nlist table inet jaco\n", true},
		{"absent table minimal", "Error: No such file or directory\n", true},
		{"empty stderr", "", false},
		{"syntax error", "Error: syntax error, unexpected newline\n", false},
		{"permission denied", "Error: Operation not permitted\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNftTableNotFound([]byte(tc.stderr)); got != tc.want {
				t.Errorf("isNftTableNotFound(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
