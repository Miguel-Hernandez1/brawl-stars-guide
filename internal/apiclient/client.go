package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const baseURL = "https://api.brawlstars.com/v1"

// Client is a rate-limited HTTP client for the Brawl Stars API.
// Keys are IP-restricted: the key only works from IPs registered at developer.brawlstars.com.
type Client struct {
	token      string
	httpClient *http.Client
}

// New creates a Client with a 10-second HTTP timeout.
// token is the Bearer token from developer.brawlstars.com (BRAWLSTARS_API_TOKEN).
func New(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// get performs a GET request to the given path (e.g. "/players/%23ABC123")
// and decodes the JSON response into dest.
func (c *Client) get(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Path: path}
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// APIError is returned when the Brawl Stars API responds with a non-200 status.
type APIError struct {
	StatusCode int
	Path       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("brawl stars API: %d on %s", e.StatusCode, e.Path)
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// IsTooManyRequests reports whether err is a 429 from the API.
func IsTooManyRequests(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusTooManyRequests
}

// IsUnauthorized reports whether err is a 401 from the API.
// A 401 means the API key is invalid or missing. This is a key-level failure
// that requires operator action; the crawler must halt globally, not retry.
func IsUnauthorized(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusUnauthorized
}

// IsForbidden reports whether err is a 403 from the API.
// A 403 means the API key is valid but the requesting IP is not whitelisted,
// or the key was revoked. This is a key-level failure requiring operator action.
func IsForbidden(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusForbidden
}

// encodedTag converts a player tag (with or without '#') to the URL-encoded form
// the API expects, e.g. "#ABC123" → "%23ABC123".
func encodedTag(tag string) string {
	tag = strings.ToUpper(strings.TrimPrefix(tag, "#"))
	return "%23" + tag
}

// ParseBattleTime parses the Brawl Stars non-standard timestamp format.
// Format: "20240901T143022.000Z" (always UTC, second precision + milliseconds).
func ParseBattleTime(s string) (time.Time, error) {
	withoutZ := strings.TrimSuffix(s, "Z")
	t, err := time.ParseInLocation("20060102T150405.000", withoutZ, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse battle time %q: %w", s, err)
	}
	return t, nil
}

// NormalizeTag strips the leading '#' and uppercases a player tag.
// All storage uses normalized tags (without '#').
func NormalizeTag(tag string) string {
	return strings.ToUpper(strings.TrimPrefix(tag, "#"))
}
