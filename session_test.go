package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNestedString(t *testing.T) {
	m := map[string]interface{}{
		"device": map[string]interface{}{
			"os": "IOS",
		},
		"links": map[string]interface{}{
			"vusbUrl": "wss://example/forward",
		},
	}

	if got := nestedString(m, "device", "os"); got != "IOS" {
		t.Errorf("device.os = %q, want IOS", got)
	}
	if got := nestedString(m, "links", "vusbUrl"); got != "wss://example/forward" {
		t.Errorf("links.vusbUrl mismatch: %q", got)
	}
	if got := nestedString(m, "device", "missing"); got != "" {
		t.Errorf("missing leaf should return empty, got %q", got)
	}
	if got := nestedString(m, "no", "such", "path"); got != "" {
		t.Errorf("missing path should return empty, got %q", got)
	}
}

func TestFetchSessionOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rdc/v2/sessions/abc-123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Basic xyz" {
			t.Errorf("Authorization = %q, want %q", got, "Basic xyz")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"state":"ACTIVE","device":{"os":"IOS"},"links":{"vusbUrl":"wss://x"}}`))
	}))
	defer srv.Close()

	info, err := fetchSession(context.Background(), srv.URL, "abc-123", "Basic xyz")
	if err != nil {
		t.Fatalf("fetchSession: %v", err)
	}
	if state, _ := info["state"].(string); state != "ACTIVE" {
		t.Errorf("state = %v", info["state"])
	}
	if osKind := nestedString(info, "device", "os"); osKind != "IOS" {
		t.Errorf("device.os = %q", osKind)
	}
}

func TestResolveAPIURL(t *testing.T) {
	cases := []struct {
		name        string
		apiURL      string
		region      string
		want        string
		wantWarning bool
		wantErr     bool
	}{
		{"api-url wins over region", "https://staging.example", "US", "https://staging.example", true, false},
		{"api-url alone", "https://api.us-west-1.saucelabs.com", "", "https://api.us-west-1.saucelabs.com", false, false},
		{"region US", "", "US", "https://api.us-west-1.saucelabs.com", false, false},
		{"region US_EAST", "", "US_EAST", "https://api.us-east-4.saucelabs.com", false, false},
		{"region EU", "", "EU", "https://api.eu-central-1.saucelabs.com", false, false},
		{"region lowercase", "", "us", "https://api.us-west-1.saucelabs.com", false, false},
		{"region mixed case", "", "Us_East", "https://api.us-east-4.saucelabs.com", false, false},
		{"region with whitespace", "", "  EU  ", "https://api.eu-central-1.saucelabs.com", false, false},
		{"api-url whitespace ignored", "   ", "EU", "https://api.eu-central-1.saucelabs.com", false, false},
		{"both set warns", "https://api.x", "eu", "https://api.x", true, false},
		{"neither set", "", "", "", false, true},
		{"unknown region", "", "APAC", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning, err := resolveAPIURL(tc.apiURL, tc.region)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got url=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveAPIURL(%q, %q) url = %q, want %q", tc.apiURL, tc.region, got, tc.want)
			}
			if tc.wantWarning {
				if warning == "" {
					t.Errorf("expected a warning when both api-url and region are set; got empty string")
				}
				// Sanity-check the warning content names the ignored region.
				if !strings.Contains(strings.ToUpper(warning), strings.ToUpper(strings.TrimSpace(tc.region))) {
					t.Errorf("warning %q should mention region %q", warning, tc.region)
				}
			} else if warning != "" {
				t.Errorf("did not expect a warning, got %q", warning)
			}
		})
	}
}

func TestFetchSessionHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetchSession(context.Background(), srv.URL, "abc-123", "Basic xyz")
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestVirtualUsbURL(t *testing.T) {
	links := func(kv ...string) map[string]interface{} {
		l := map[string]interface{}{}
		for i := 0; i < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		return map[string]interface{}{"links": l}
	}

	cases := []struct {
		name    string
		info    map[string]interface{}
		want    string
		wantErr bool
	}{
		{
			name: "private android session",
			info: links("adbUrl", "wss://example/forward", "vusbUrl", "wss://example/forward"),
			want: "wss://example/forward",
		},
		{
			// iOS dials vusbUrl (…/forward), not usbmuxdUrl (…/usbmuxd).
			name: "private ios session",
			info: links("usbmuxdUrl", "wss://example/usbmuxd", "vusbUrl", "wss://example/forward"),
			want: "wss://example/forward",
		},
		// A public device drops all three links.
		{name: "public device session", info: links(), wantErr: true},
		{name: "no links at all", info: map[string]interface{}{}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := virtualUsbURL(tc.info)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got url=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("virtualUsbURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoVirtualUsbErrorMessage(t *testing.T) {
	// The message has to name the cause, the fix, and the links the user
	// would have gone looking for — otherwise it is just another 404.
	msg := errNoVirtualUsb.Error()
	for _, want := range []string{"public device", "private device", "adbUrl", "usbmuxdUrl", "vusbUrl", "docs.saucelabs.com"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}
