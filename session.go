package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// regionURLs maps the user-facing region tokens (case-insensitive) to the
// matching Sauce Labs REST API base URL. Keep entries here in sync with
// the README's "Flags reference" table.
var regionURLs = map[string]string{
	"US":      "https://api.us-west-1.saucelabs.com",
	"US_EAST": "https://api.us-east-4.saucelabs.com",
	"EU":      "https://api.eu-central-1.saucelabs.com",
}

// validRegions is the canonical order we list in error messages.
var validRegions = []string{"US", "US_EAST", "EU"}

// A variable rather than a constant so tests need not sleep for real.
var waitPollInterval = 5 * time.Second

// resolveAPIURL determines which REST API base URL to use given the two
// possible inputs:
//
//   - apiURL : explicit --api-url / SAUCE_API_URL value (wins when set)
//   - region : --region / SAUCE_REGION value (used otherwise)
//
// It returns the resolved URL and, when both inputs were supplied, a
// non-empty warning string that the caller is expected to surface to the
// user (so they know --region was silently ignored). The error is non-nil
// only if neither input is set or the region value is unknown.
func resolveAPIURL(apiURL, region string) (url, warning string, err error) {
	apiURL = strings.TrimSpace(apiURL)
	region = strings.ToUpper(strings.TrimSpace(region))
	if apiURL != "" {
		if region != "" {
			// Phrased without flag names because either value may have
			// come from its environment variable instead of the matching
			// command-line flag.
			warning = fmt.Sprintf("explicit API URL is set; ignoring region %s", region)
		}
		return apiURL, warning, nil
	}
	if region == "" {
		return "", "", errors.New("set --region (SAUCE_REGION) or --api-url (SAUCE_API_URL); one of them is required")
	}
	u, ok := regionURLs[region]
	if !ok {
		return "", "", fmt.Errorf("unknown region %q; valid values: %s", region, strings.Join(validRegions, ", "))
	}
	return u, "", nil
}

// fetchSession does one GET /rdc/v2/sessions/{id} and returns the parsed
// JSON response.
func fetchSession(ctx context.Context, apiURL, sessionID, authHeader string) (map[string]interface{}, error) {
	url := strings.TrimRight(apiURL, "/") + "/rdc/v2/sessions/" + sessionID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authHeader)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, body)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// waitForActive polls the session until it enters the ACTIVE state, or
// returns immediately if it's in any state it can no longer leave.
func waitForActive(ctx context.Context, apiURL, sessionID, authHeader string) (map[string]interface{}, error) {
	for {
		info, err := fetchSession(ctx, apiURL, sessionID, authHeader)
		if err != nil {
			return nil, err
		}
		state, _ := info["state"].(string)
		switch state {
		case "ACTIVE":
			return info, nil
		case "PENDING", "CREATING":
			// CREATING follows PENDING once a device has been picked; both are
			// still on the way to ACTIVE.
			log.Printf("Session is %s, retrying in %s...", state, waitPollInterval)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(waitPollInterval):
			}
		default:
			// error.message is the only thing that explains a failed allocation.
			if message := nestedString(info, "error", "message"); message != "" {
				return nil, fmt.Errorf("session not active: state=%q: %s", state, message)
			}
			return nil, fmt.Errorf("session not active: state=%q", state)
		}
	}
}

// nestedString walks the JSON map by successive keys and returns the
// string at the end (or empty string if anything along the path is missing
// or the leaf is not a string).
func nestedString(m map[string]interface{}, keys ...string) string {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}
