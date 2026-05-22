package main

import (
	"strings"
	"testing"
)

func TestIOSSupportError(t *testing.T) {
	cases := []struct {
		goos    string
		wantErr bool
	}{
		{"darwin", false},
		{"linux", false},
		{"windows", true},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			err := iosSupportError(tc.goos)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for goos=%q", tc.goos)
				}
				// Message should name the missing piece and point at the
				// supported hosts so users aren't left guessing.
				msg := err.Error()
				for _, want := range []string{"Windows", "usbmuxd", "macOS", "Linux"} {
					if !strings.Contains(msg, want) {
						t.Errorf("error message missing %q: %s", want, msg)
					}
				}
			} else if err != nil {
				t.Errorf("unexpected error for goos=%q: %v", tc.goos, err)
			}
		})
	}
}
