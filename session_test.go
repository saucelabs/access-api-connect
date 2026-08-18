package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// fakeSessionStates serves the given bodies in order, one per request, so a test
// can walk a session through a sequence of states.
func fakeSessionStates(t *testing.T, bodies ...string) string {
	t.Helper()

	var (
		mu   sync.Mutex
		next int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body := "{}"
		if next < len(bodies) {
			body, next = bodies[next], next+1
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// CREATING sits between PENDING and ACTIVE and real sessions pass through it, so
// treating it as terminal fails a session that would have worked.
func TestWaitForActivePassesThroughCreating(t *testing.T) {
	restore := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = restore })

	url := fakeSessionStates(t,
		`{"id":"abc","state":"CREATING"}`,
		`{"id":"abc","state":"ACTIVE","links":{"vusbUrl":"wss://example.invalid/vusb"}}`,
	)

	info, err := waitForActive(context.Background(), url, "abc", "Basic x")
	if err != nil {
		t.Fatalf("waitForActive: %v", err)
	}
	if got := nestedString(info, "links", "vusbUrl"); got != "wss://example.invalid/vusb" {
		t.Errorf("vusbUrl = %q, want the url from the ACTIVE response", got)
	}
}

func TestWaitForActiveReportsTheErrorMessage(t *testing.T) {
	url := fakeSessionStates(t,
		`{"id":"abc","state":"ERRORED","error":{"message":"There is no device that matches the query"}}`,
	)

	_, err := waitForActive(context.Background(), url, "abc", "Basic x")
	if err == nil {
		t.Fatal("want an error for an ERRORED session")
	}
	// Without it, a failed allocation is indistinguishable from any other failure.
	if !strings.Contains(err.Error(), "no device that matches the query") {
		t.Errorf("error %q drops the API's explanation", err)
	}
}
