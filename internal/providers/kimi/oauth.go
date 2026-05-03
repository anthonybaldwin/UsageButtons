// Auth fallback for Kimi for Coding when the Helper extension is
// unavailable. Uses an OAuth token grant that the `kimi login` CLI
// places at ~/.kimi/credentials/kimi-code.json, refreshes it against
// auth.kimi.com when within 5 minutes of expiry (or on a 401/403),
// and reads usage from api.kimi.com/coding/v1/usages.
//
// The cookie path remains primary (extension-first architecture);
// OAuth is only consulted when HostAvailable is false or when the
// cookie path returns an auth-stale error.

package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	// oauthClientID is the Kimi-published OAuth client used by the
	// `kimi login` CLI. Refresh-only — initial auth-code grant + PKCE
	// is handled by the CLI; we never see the user's password.
	oauthClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	// defaultOAuthRefreshURL is the form-encoded refresh-token endpoint.
	defaultOAuthRefreshURL = "https://auth.kimi.com/api/oauth/token"
	// oauthUsageURL returns Kimi for Coding session/weekly windows.
	oauthUsageURL = "https://api.kimi.com/coding/v1/usages"
	// oauthRefreshBuffer is how far ahead of expiry we proactively refresh.
	oauthRefreshBuffer = 5 * time.Minute
	// oauthHTTPTimeout caps refresh + usage HTTP calls.
	oauthHTTPTimeout = 30 * time.Second
)

// oauthCredsPathFn is overridden in tests to redirect the credential
// lookup at a temp dir without depending on HOME/USERPROFILE quirks.
var oauthCredsPathFn = defaultOAuthCredsPath

// oauthRefreshURLFn is overridden in tests to point refresh requests at
// a httptest server.
var oauthRefreshURLFn = func() string { return defaultOAuthRefreshURL }

// defaultOAuthCredsPath returns the standard kimi-code OAuth path.
func defaultOAuthCredsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kimi", "credentials", "kimi-code.json")
}

// oauthCredsPath returns the on-disk location of the kimi-code OAuth blob.
func oauthCredsPath() string { return oauthCredsPathFn() }

// oauthCreds is the on-disk shape of ~/.kimi/credentials/kimi-code.json.
// Fields not listed here (if Kimi's CLI ever adds new ones) are preserved
// across refresh because saveOAuthCreds round-trips through map[string]any.
type oauthCreds struct {
	AccessToken  string   `json:"access_token,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresAt    *float64 `json:"expires_at,omitempty"` // unix seconds (may be fractional)
	Scope        string   `json:"scope,omitempty"`
	TokenType    string   `json:"token_type,omitempty"`
}

// loadOAuthCreds reads and validates the kimi-code credential blob.
func loadOAuthCreds() (oauthCreds, error) {
	path := oauthCredsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return oauthCreds{}, fmt.Errorf("Kimi credentials not found at %s. Run `kimi login` to authenticate.", path)
		}
		return oauthCreds{}, err
	}
	var c oauthCreds
	if err := json.Unmarshal(data, &c); err != nil {
		return oauthCreds{}, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	if strings.TrimSpace(c.AccessToken) == "" && strings.TrimSpace(c.RefreshToken) == "" {
		return oauthCreds{}, fmt.Errorf("Kimi credentials at %s missing access_token / refresh_token. Run `kimi login` to authenticate.", path)
	}
	return c, nil
}

// needsRefresh reports whether the access token is missing or within
// oauthRefreshBuffer of its expiry.
func (c oauthCreds) needsRefresh(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return true
	}
	if c.ExpiresAt == nil {
		return true
	}
	return now.Add(oauthRefreshBuffer).Unix() >= int64(*c.ExpiresAt)
}

// refreshResponse is the JSON shape of auth.kimi.com's token endpoint.
type refreshResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token,omitempty"`
	ExpiresIn    *int64  `json:"expires_in,omitempty"`
	Scope        string  `json:"scope,omitempty"`
	TokenType    string  `json:"token_type,omitempty"`
}

// refreshOAuthToken exchanges the stored refresh_token for a fresh
// access_token and persists the result. On non-2xx responses other than
// 401/403 the existing access token is kept and the caller proceeds —
// matching openusage's leniency for transient refresh failures.
func refreshOAuthToken(ctx context.Context, creds oauthCreds) (oauthCreds, error) {
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return creds, fmt.Errorf("Kimi credentials missing refresh_token. Run `kimi login` to authenticate.")
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", creds.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthRefreshURLFn(), strings.NewReader(form.Encode()))
	if err != nil {
		return creds, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)

	client := &http.Client{Timeout: oauthHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return creds, fmt.Errorf("Kimi OAuth refresh network error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return creds, fmt.Errorf("Kimi OAuth refresh read error: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return creds, fmt.Errorf("Kimi session expired. Run `kimi login` to authenticate.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Non-auth failure: keep existing token and let the usage call try.
		return creds, nil
	}
	var decoded refreshResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return creds, fmt.Errorf("Kimi OAuth refresh parse error: %w", err)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return creds, fmt.Errorf("Kimi OAuth refresh missing access_token")
	}
	creds.AccessToken = strings.TrimSpace(decoded.AccessToken)
	if strings.TrimSpace(decoded.RefreshToken) != "" {
		creds.RefreshToken = strings.TrimSpace(decoded.RefreshToken)
	}
	if decoded.ExpiresIn != nil {
		exp := float64(time.Now().Unix() + *decoded.ExpiresIn)
		creds.ExpiresAt = &exp
	}
	if strings.TrimSpace(decoded.Scope) != "" {
		creds.Scope = strings.TrimSpace(decoded.Scope)
	}
	if strings.TrimSpace(decoded.TokenType) != "" {
		creds.TokenType = strings.TrimSpace(decoded.TokenType)
	}
	if err := saveOAuthCreds(creds); err != nil {
		return creds, fmt.Errorf("save Kimi credentials: %w", err)
	}
	return creds, nil
}

// saveOAuthCreds writes refreshed credentials back to disk while
// preserving any extra keys the Kimi CLI may have added since.
func saveOAuthCreds(creds oauthCreds) error {
	path := oauthCredsPath()
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	root["access_token"] = creds.AccessToken
	if creds.RefreshToken != "" {
		root["refresh_token"] = creds.RefreshToken
	}
	if creds.ExpiresAt != nil {
		root["expires_at"] = *creds.ExpiresAt
	}
	if creds.Scope != "" {
		root["scope"] = creds.Scope
	}
	if creds.TokenType != "" {
		root["token_type"] = creds.TokenType
	}
	return providerutil.WriteJSONAtomic(path, root)
}

// oauthUsageEnvelope is the api.kimi.com/coding/v1/usages response shape.
// Differs from the gateway shape parsed by parseUsage in kimi.go (which
// returns {usages:[{scope,detail,limits[]}]}) — the OAuth-direct path
// returns a single top-level usage block plus a windowed limits[] array.
type oauthUsageEnvelope struct {
	Usage  *usageDetail     `json:"usage,omitempty"`
	Limits []rateLimitEntry `json:"limits,omitempty"`
}

// fetchWithOAuth refreshes credentials when needed, then reads the
// kimi-code usage endpoint and normalizes it into a usageSnapshot
// compatible with the cookie path's snapshotFromUsage().
func fetchWithOAuth(ctx context.Context) (usageSnapshot, error) {
	creds, err := loadOAuthCreds()
	if err != nil {
		return usageSnapshot{}, err
	}
	if creds.needsRefresh(time.Now()) {
		creds, err = refreshOAuthToken(ctx, creds)
		if err != nil {
			return usageSnapshot{}, err
		}
	}
	usage, fetchErr := readOAuthUsage(ctx, creds.AccessToken)
	if fetchErr == nil {
		return usage, nil
	}
	// Reactive refresh on auth failure, retry once.
	var httpErr *httputil.Error
	if errors.As(fetchErr, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
		refreshed, refreshErr := refreshOAuthToken(ctx, creds)
		if refreshErr != nil {
			return usageSnapshot{}, refreshErr
		}
		usage, fetchErr = readOAuthUsage(ctx, refreshed.AccessToken)
		if fetchErr == nil {
			return usage, nil
		}
	}
	return usageSnapshot{}, fetchErr
}

// readOAuthUsage performs the GET against api.kimi.com/coding/v1/usages.
func readOAuthUsage(ctx context.Context, accessToken string) (usageSnapshot, error) {
	if strings.TrimSpace(accessToken) == "" {
		return usageSnapshot{}, fmt.Errorf("Kimi OAuth access token empty after refresh")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthUsageURL, nil)
	if err != nil {
		return usageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)

	client := &http.Client{Timeout: oauthHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("Kimi usage fetch error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("Kimi usage read error: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usageSnapshot{}, &httputil.Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        oauthUsageURL,
		}
	}
	var env oauthUsageEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return usageSnapshot{}, fmt.Errorf("invalid Kimi usage JSON: %w", err)
	}
	return parseOAuthUsage(env, time.Now().UTC())
}

// parseOAuthUsage maps the kimi-code envelope into the same usageSnapshot
// shape produced by parseUsage so snapshotFromUsage doesn't care which
// transport produced the data.
func parseOAuthUsage(env oauthUsageEnvelope, now time.Time) (usageSnapshot, error) {
	if env.Usage == nil && len(env.Limits) == 0 {
		return usageSnapshot{}, fmt.Errorf("Kimi response missing usage and limits")
	}
	snap := usageSnapshot{UpdatedAt: now}
	if env.Usage != nil {
		snap.Weekly = *env.Usage
	}
	if len(env.Limits) > 0 {
		detail := env.Limits[0].Detail
		snap.Rate = &detail
	}
	return snap, nil
}

