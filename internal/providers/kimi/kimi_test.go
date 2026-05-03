package kimi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseUsageMapsFeatureCoding verifies the cookie-path parser keeps
// returning weekly + 5-hour rate from a FEATURE_CODING usage entry.
func TestParseUsageMapsFeatureCoding(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	resp := usageResponse{Usages: []usageEntry{{
		Scope: "FEATURE_CODING",
		Detail: usageDetail{
			Limit:     "100",
			Remaining: "74",
			ResetTime: "2026-05-08T12:00:00Z",
		},
		Limits: []rateLimitEntry{{
			Window: rateWindow{Duration: 300, TimeUnit: "TIME_UNIT_MINUTE"},
			Detail: usageDetail{Limit: "20", Remaining: "5", ResetTime: "2026-05-01T15:00:00Z"},
		}},
	}}}
	snap, err := parseUsage(resp, now)
	if err != nil {
		t.Fatalf("parseUsage error: %v", err)
	}
	if snap.Weekly.Limit != "100" || snap.Weekly.Remaining != "74" {
		t.Fatalf("weekly = %+v", snap.Weekly)
	}
	if snap.Rate == nil || snap.Rate.Limit != "20" || snap.Rate.Remaining != "5" {
		t.Fatalf("rate = %+v", snap.Rate)
	}
}

// TestParseOAuthUsageMapsEnvelope verifies the OAuth-direct envelope
// (top-level usage + limits[]) flattens into the same usageSnapshot
// shape as the cookie path so snapshotFromUsage can be reused.
func TestParseOAuthUsageMapsEnvelope(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	env := oauthUsageEnvelope{
		Usage: &usageDetail{Limit: "100", Used: "26", ResetTime: "2026-05-08T12:00:00Z"},
		Limits: []rateLimitEntry{{
			Window: rateWindow{Duration: 300, TimeUnit: "TIME_UNIT_MINUTE"},
			Detail: usageDetail{Limit: "20", Used: "15", ResetTime: "2026-05-01T15:00:00Z"},
		}},
	}
	snap, err := parseOAuthUsage(env, now)
	if err != nil {
		t.Fatalf("parseOAuthUsage error: %v", err)
	}
	if snap.Weekly.Limit != "100" || snap.Weekly.Used != "26" {
		t.Fatalf("weekly = %+v", snap.Weekly)
	}
	if snap.Rate == nil || snap.Rate.Used != "15" {
		t.Fatalf("rate = %+v", snap.Rate)
	}
}

// TestParseOAuthUsageEmptyResponseErrors guards against a 200 with
// neither a usage block nor a limits[] array — happens when the
// account hasn't been provisioned for Kimi for Coding.
func TestParseOAuthUsageEmptyResponseErrors(t *testing.T) {
	_, err := parseOAuthUsage(oauthUsageEnvelope{}, time.Now())
	if err == nil {
		t.Fatal("expected error for empty envelope, got nil")
	}
}

// TestNeedsRefresh verifies the 5-minute proactive refresh window and
// the missing-token early-out both behave correctly.
func TestNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	exp := func(offset time.Duration) *float64 {
		v := float64(now.Add(offset).Unix())
		return &v
	}

	tests := []struct {
		name  string
		creds oauthCreds
		want  bool
	}{
		{"missing access token", oauthCreds{RefreshToken: "r", ExpiresAt: exp(time.Hour)}, true},
		{"missing expires_at", oauthCreds{AccessToken: "a", RefreshToken: "r"}, true},
		{"within buffer", oauthCreds{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp(2 * time.Minute)}, true},
		{"already expired", oauthCreds{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp(-time.Hour)}, true},
		{"fresh", oauthCreds{AccessToken: "a", RefreshToken: "r", ExpiresAt: exp(time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.creds.needsRefresh(now); got != tt.want {
				t.Fatalf("needsRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoadOAuthCredsMissingFile guards the user-facing message path so
// `kimi login` is mentioned when credentials don't exist yet.
func TestLoadOAuthCredsMissingFile(t *testing.T) {
	withTempCredsDir(t)
	_, err := loadOAuthCreds()
	if err == nil {
		t.Fatal("expected error for missing creds file")
	}
	if !strings.Contains(err.Error(), "kimi login") {
		t.Fatalf("error %q does not mention `kimi login`", err.Error())
	}
}

// TestLoadOAuthCredsParsesValidFile verifies fractional expires_at,
// optional scope, and optional token_type round-trip cleanly.
func TestLoadOAuthCredsParsesValidFile(t *testing.T) {
	dir := withTempCredsDir(t)
	body := `{
		"access_token": "AT",
		"refresh_token": "RT",
		"expires_at": 1769861835.261056,
		"scope": "kimi-code",
		"token_type": "Bearer"
	}`
	writeCreds(t, dir, body)
	creds, err := loadOAuthCreds()
	if err != nil {
		t.Fatalf("loadOAuthCreds error: %v", err)
	}
	if creds.AccessToken != "AT" || creds.RefreshToken != "RT" || creds.Scope != "kimi-code" {
		t.Fatalf("creds = %+v", creds)
	}
	if creds.ExpiresAt == nil || *creds.ExpiresAt < 1.7e9 {
		t.Fatalf("expires_at = %+v, want fractional unix seconds", creds.ExpiresAt)
	}
}

// TestRefreshOAuthTokenPersistsResponse checks that a 200 response
// updates access_token, optional refresh_token, expires_at, and
// preserves the existing token_type while writing back atomically.
func TestRefreshOAuthTokenPersistsResponse(t *testing.T) {
	dir := withTempCredsDir(t)
	writeCreds(t, dir, `{"access_token":"old","refresh_token":"R","token_type":"Bearer"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q", got)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != oauthClientID {
			t.Errorf("client_id = %q", r.PostForm.Get("client_id"))
		}
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("refresh_token") != "R" {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"NEW",
			"expires_in": 3600,
			"scope": "kimi-code"
		}`))
	}))
	defer srv.Close()
	withRefreshURL(t, srv.URL)

	creds, err := loadOAuthCreds()
	if err != nil {
		t.Fatalf("loadOAuthCreds: %v", err)
	}
	creds, err = refreshOAuthToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("refreshOAuthToken: %v", err)
	}
	if creds.AccessToken != "NEW" {
		t.Fatalf("access_token = %q", creds.AccessToken)
	}
	if creds.ExpiresAt == nil {
		t.Fatal("expires_at not set")
	}
	if creds.Scope != "kimi-code" {
		t.Fatalf("scope = %q", creds.Scope)
	}

	reloaded, err := loadOAuthCreds()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.AccessToken != "NEW" || reloaded.RefreshToken != "R" || reloaded.TokenType != "Bearer" {
		t.Fatalf("reloaded creds did not round-trip: %+v", reloaded)
	}
}

// TestRefreshOAuthToken401Surfaces verifies an auth-status response
// surfaces a `kimi login` hint instead of being swallowed.
func TestRefreshOAuthToken401Surfaces(t *testing.T) {
	dir := withTempCredsDir(t)
	writeCreds(t, dir, `{"access_token":"old","refresh_token":"R"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withRefreshURL(t, srv.URL)

	creds, err := loadOAuthCreds()
	if err != nil {
		t.Fatalf("loadOAuthCreds: %v", err)
	}
	_, err = refreshOAuthToken(context.Background(), creds)
	if err == nil || !strings.Contains(err.Error(), "kimi login") {
		t.Fatalf("err = %v, want `kimi login` hint", err)
	}
}

// TestRefreshOAuthTokenTransientFailureIsLenient verifies a 502 leaves
// the existing access token in place so the usage call can still try.
func TestRefreshOAuthTokenTransientFailureIsLenient(t *testing.T) {
	dir := withTempCredsDir(t)
	writeCreds(t, dir, `{"access_token":"keep","refresh_token":"R"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	withRefreshURL(t, srv.URL)

	creds, err := loadOAuthCreds()
	if err != nil {
		t.Fatalf("loadOAuthCreds: %v", err)
	}
	got, err := refreshOAuthToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("err = %v, want nil for transient failure", err)
	}
	if got.AccessToken != "keep" {
		t.Fatalf("access_token = %q, want existing token preserved", got.AccessToken)
	}
}

// TestSnapshotFromUsageReportsKimiForCoding verifies the rebrand made
// it to the snapshot ProviderName so the SD cache key matches the new
// label.
func TestSnapshotFromUsageReportsKimiForCoding(t *testing.T) {
	snap := snapshotFromUsage(usageSnapshot{
		Weekly:    usageDetail{Limit: "100", Used: "10"},
		UpdatedAt: time.Now().UTC(),
	})
	if snap.ProviderName != "Kimi for Coding" {
		t.Fatalf("ProviderName = %q, want %q", snap.ProviderName, "Kimi for Coding")
	}
}

// withTempCredsDir redirects oauthCredsPathFn at a fresh temp dir for
// the duration of the test.
func withTempCredsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := oauthCredsPathFn
	oauthCredsPathFn = func() string { return filepath.Join(dir, "kimi-code.json") }
	t.Cleanup(func() { oauthCredsPathFn = prev })
	return dir
}

// writeCreds places a credential JSON blob at the test temp location.
func writeCreds(t *testing.T, _ string, body string) {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Fatalf("test fixture invalid JSON: %s", body)
	}
	if err := os.WriteFile(oauthCredsPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
}

// withRefreshURL redirects oauthRefreshURLFn at the supplied URL for
// the duration of the test.
func withRefreshURL(t *testing.T, url string) {
	t.Helper()
	prev := oauthRefreshURLFn
	oauthRefreshURLFn = func() string { return url }
	t.Cleanup(func() { oauthRefreshURLFn = prev })
}
