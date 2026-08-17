package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Session creation, so an importing program can take a device rather than being
// handed a session id. The CLI uses none of it.

// SessionRequest describes the device a new session should take.
type SessionRequest struct {
	// OS is required: "IOS" or "ANDROID".
	OS string

	// DeviceName pins one device. Empty takes any device matching OS.
	DeviceName string

	// SessionDuration is ISO-8601, e.g. "PT15M". Empty leaves the server
	// default. The clock starts at creation and runs while the session is still
	// PENDING, so a short duration can expire before the device is allocated.
	SessionDuration string
}

// Every field is omitempty so an OS-only request serialises to
// {"device":{"os":"…"}} — the form the API is known to accept.
type deviceQuery struct {
	OS         string `json:"os,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

type sessionConfig struct {
	SessionDuration string `json:"sessionDuration,omitempty"`
}

type createBody struct {
	Device        *deviceQuery   `json:"device,omitempty"`
	Configuration *sessionConfig `json:"configuration,omitempty"`
}

// CreateSession takes a device. The session comes back PENDING with no links;
// pass its id to WaitForActive. Only private devices can be allocated, so an org
// without any gets an ERRORED session rather than a rejected request.
func CreateSession(ctx context.Context, apiURL, authHeader string, request SessionRequest) (map[string]interface{}, error) {
	if request.OS == "" {
		return nil, errors.New("session request needs an OS (\"IOS\" or \"ANDROID\")")
	}

	body := createBody{Device: &deviceQuery{OS: request.OS, DeviceName: request.DeviceName}}
	if request.SessionDuration != "" {
		body.Configuration = &sessionConfig{SessionDuration: request.SessionDuration}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode create request: %w", err)
	}

	return doSessionRequest(ctx, http.MethodPost, sessionsURL(apiURL), authHeader, encoded)
}

// CloseSession gives the device back. Call it even when the session never became
// usable: one still allocating holds its device until it expires.
func CloseSession(ctx context.Context, apiURL, sessionID, authHeader string, rebootDevice bool) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/%s?rebootDevice=%t", sessionsURL(apiURL), sessionID, rebootDevice)
	return doSessionRequest(ctx, http.MethodDelete, url, authHeader, nil)
}

func sessionsURL(apiURL string) string {
	return strings.TrimRight(apiURL, "/") + "/rdc/v2/sessions"
}

func doSessionRequest(ctx context.Context, method, url, authHeader string, body []byte) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s %s: %s", resp.StatusCode, method, url, snippet(payload))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("decode %s %s response: %w (body: %s)", method, url, err, snippet(payload))
	}
	return result, nil
}

// snippet keeps errors readable when something upstream returns an HTML page.
func snippet(payload []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return "<empty body>"
	}
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
