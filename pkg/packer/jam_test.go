package packer

import "testing"

func TestParseVersion(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"jam 2.18.0\n", "2.18.0"},
		{"jam v2.18.0", "2.18.0"},
		{"2.18.0", "2.18.0"},
		// Binaries produced by `go install` carry no version stamp.
		{"jam \n", ""},
		{"", ""},
		{"jam unknown", ""},
	} {
		if got := parseVersion(tt.in); got != tt.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	for _, tt := range []struct {
		have, want string
		ok         bool
	}{
		{"2.18.0", "2.0.0", true},
		{"2.0.0", "2.0.0", true},
		{"1.7.0", "2.0.0", false},
		{"2.1", "2.0.0", true},
		{"2", "2.0.0", true},
		{"10.0.0", "2.0.0", true},
		{"1.99.99", "2.0.0", false},
	} {
		if got := atLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("atLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}
