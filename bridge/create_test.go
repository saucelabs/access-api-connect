package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

// fakeSessionAPI serves the responses in order, one per request, and records
// what it was asked.
func fakeSessionAPI(t *testing.T, responses ...string) (baseURL string, seen *[]recordedRequest) {
	t.Helper()

	var (
		mu       sync.Mutex
		recorded []recordedRequest
		next     int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		recorded = append(recorded, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Body:   strings.TrimSpace(string(body)),
		})
		response := "{}"
		if next < len(responses) {
			response = responses[next]
			next++
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	return server.URL, &recorded
}

func TestCreateSessionSendsOnlyTheFieldsThatAreSet(t *testing.T) {
	baseURL, seen := fakeSessionAPI(t, `{"id":"abc","state":"PENDING"}`)

	session, err := CreateSession(context.Background(), baseURL, "Basic dXNlcjprZXk=", SessionRequest{OS: "ios"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id, _ := session["id"].(string); id != "abc" {
		t.Errorf("id = %q, want %q", id, "abc")
	}

	if len(*seen) != 1 {
		t.Fatalf("got %d requests, want 1", len(*seen))
	}
	request := (*seen)[0]
	if request.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", request.Method)
	}
	if request.Path != "/rdc/v2/sessions" {
		t.Errorf("path = %s, want /rdc/v2/sessions", request.Path)
	}
	if request.Auth != "Basic dXNlcjprZXk=" {
		t.Errorf("authorization = %q, not forwarded", request.Auth)
	}
	// Sending an empty sessionDuration is not the same as leaving the default.
	if request.Body != `{"device":{"os":"ios"}}` {
		t.Errorf("body = %s, want {\"device\":{\"os\":\"ios\"}}", request.Body)
	}
}

func TestCreateSessionIncludesDeviceNameAndDuration(t *testing.T) {
	baseURL, seen := fakeSessionAPI(t, `{"id":"abc","state":"PENDING"}`)

	_, err := CreateSession(context.Background(), baseURL, "Basic x", SessionRequest{
		OS:              "ANDROID",
		DeviceName:      "Google_Pixel_8_real",
		SessionDuration: "PT15M",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var body createBody
	if err := json.Unmarshal([]byte((*seen)[0].Body), &body); err != nil {
		t.Fatalf("decode recorded body %s: %v", (*seen)[0].Body, err)
	}
	if body.Device == nil || body.Device.DeviceName != "Google_Pixel_8_real" {
		t.Errorf("device = %+v, want deviceName Google_Pixel_8_real", body.Device)
	}
	if body.Configuration == nil || body.Configuration.SessionDuration != "PT15M" {
		t.Errorf("configuration = %+v, want sessionDuration PT15M", body.Configuration)
	}
}

func TestCreateSessionRequiresAnOS(t *testing.T) {
	if _, err := CreateSession(context.Background(), "http://unused.invalid", "Basic x", SessionRequest{}); err == nil {
		t.Fatal("CreateSession with no OS returned nil error, want a validation failure")
	}
}

func TestCreateSessionSurfacesHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Authorization failed"}`))
	}))
	t.Cleanup(server.Close)

	_, err := CreateSession(context.Background(), server.URL, "Basic bad", SessionRequest{OS: "ios"})
	if err == nil {
		t.Fatal("want an error for HTTP 401")
	}
	// The body says more than the status: empty credentials look like this.
	if !strings.Contains(err.Error(), "Authorization failed") {
		t.Errorf("error %q does not include the response body", err)
	}
}

func TestCloseSessionPassesRebootFlag(t *testing.T) {
	baseURL, seen := fakeSessionAPI(t, `{"id":"abc","state":"CLOSING"}`)

	if _, err := CloseSession(context.Background(), baseURL, "abc", "Basic x", true); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	request := (*seen)[0]
	if request.Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", request.Method)
	}
	if request.Path != "/rdc/v2/sessions/abc" {
		t.Errorf("path = %s, want /rdc/v2/sessions/abc", request.Path)
	}
	if request.Query != "rebootDevice=true" {
		t.Errorf("query = %q, want rebootDevice=true", request.Query)
	}
}

// CREATING sits between PENDING and ACTIVE; treating it as terminal fails a
// session that would have worked.
func TestWaitForActivePassesThroughCreating(t *testing.T) {
	restore := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = restore })

	baseURL, _ := fakeSessionAPI(t,
		`{"id":"abc","state":"CREATING"}`,
		`{"id":"abc","state":"ACTIVE","links":{"vusbUrl":"wss://example.invalid/vusb"}}`,
	)

	session, err := WaitForActive(context.Background(), baseURL, "abc", "Basic x")
	if err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}
	if url := NestedString(session, "links", "vusbUrl"); url != "wss://example.invalid/vusb" {
		t.Errorf("vusbUrl = %q, want the url from the ACTIVE response", url)
	}
}

func TestWaitForActiveReportsTheErrorMessage(t *testing.T) {
	baseURL, _ := fakeSessionAPI(t,
		`{"id":"abc","state":"ERRORED","error":{"message":"There is no device that matches the query"}}`,
	)

	_, err := WaitForActive(context.Background(), baseURL, "abc", "Basic x")
	if err == nil {
		t.Fatal("want an error for an ERRORED session")
	}
	// Without it, "no private devices" is indistinguishable from any other failure.
	if !strings.Contains(err.Error(), "no device that matches the query") {
		t.Errorf("error %q drops the API's explanation", err)
	}
}
