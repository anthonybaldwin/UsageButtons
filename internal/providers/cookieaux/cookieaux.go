// Package cookieaux holds provider-facing helpers for the browser
// fetch-proxy path. It's a thin layer over internal/cookies so
// cookie-gated providers (Claude extras, Cursor, Ollama) share
// consistent error messages without each one importing the low-level
// package.
//
// Providers typically do:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
//	defer cancel()
//	if !cookies.HostAvailable(ctx) {
//	    return snapshot_with_missing_message
//	}
//	err := cookies.FetchJSON(ctx, url, nil, &out)
//	// inspect err for *httputil.Error to distinguish stale auth
//
// cookieaux.MissingMessage / StaleMessage give both places a single
// source of truth for the user-facing strings.
package cookieaux

import "fmt"

// MissingMessage returns the Snapshot.Error text when the extension
// isn't connected. providerLabel is the site the user would sign in
// to (e.g. "cursor.com").
func MissingMessage(providerLabel string) string {
	return fmt.Sprintf(
		"Install the Usage Buttons Helper Chrome extension to pull %s usage from your logged-in browser session.",
		providerLabel,
	)
}

// StaleMessage returns the Snapshot.Error text when a request came
// back 401/403 — almost always "you're signed out of the provider in
// Chrome."
func StaleMessage(providerLabel string) string {
	return fmt.Sprintf(
		"Your %s browser session is signed out or expired. Sign in again in Chrome, then refresh.",
		providerLabel,
	)
}
