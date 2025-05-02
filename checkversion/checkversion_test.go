package checkversion

import (
	"testing"
)

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		minUaVer   string
		userAgent  string
		shouldFail bool
	}{
		{"0.3.14", "/spectred:0.3.14/spectred:0.3.14/", false},
		{"0.3.14", "/spectred:0.3.14/spectred:0.0.0/", false},
		{"0.3.14", "/spectred:0.3.14/spectred:0.3.14(spectre-desktop_0.3.14)/", false},
		{"0.3.14", "/spectred:0.3.19/spectred:0.3.19/", false},
		{"0.3.14", "/spectred:1.1.0/", false},

		{"0.3.57", "/spectred:0.3.14/spectred:0.3.14/", true},
		{"0.3.57", "/spectred:0.3.14/spectred:0.0.0/", true},
		{"0.3.57", "/spectred:0.3.14/spectred:0.3.14(spectre-desktop_0.3.14)/", true},
		{"0.3.57", "/spectred:0.3.57/spectred:0.3.57/", false},
		{"0.3.57", "/spectred:1.1.0/", false},

		{"1.0.0", "/spectred:0.3.14/spectred:0.3.14/", true},
		{"1.0.0", "/spectred:0.3.14/spectred:0.0.0/", true},
		{"1.0.0", "/spectred:0.3.14/spectred:0.3.14(spectre-desktop_0.3.14)/", true},
		{"1.0.0", "/spectred:0.18.9/spectred:0.18.9/", true},
		{"1.0.0", "/spectred:1.1.0/", false},
	}

	for _, tt := range tests {
		err := CheckVersion(tt.minUaVer, tt.userAgent)
		if tt.shouldFail && err == nil {
			t.Errorf("Expected failure for %q with %q, but got nil", tt.minUaVer, tt.userAgent)
		}
		if !tt.shouldFail && err != nil {
			t.Errorf("Unexpected error for %q with %q: %v", tt.minUaVer, tt.userAgent, err)
		}
	}
}
