// Package cookieaux is the decision layer cookie-gated providers use
// to route a request through either the companion browser extension
// (preferred) or the user's manually pasted cookie header (fallback).
//
// The rule: if the extension has handshaken this session, use it —
// Chrome's real TLS + UA + cookie jar, cookies never leave the
// browser. Else if a manual paste is configured, fall back to
// httputil with the Cookie header set. Else the Fetcher reports
// Available=false and the caller MUST return a "waiting on browser"
// snapshot without firing a request.
package cookieaux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

// Source identifies which transport a Fetcher used for the most
// recent call — exposed so providers can tailor error messages
// ("refresh browser session" vs "paste a fresh cookie").
type Source string

const (
	SourceNone      Source = ""
	SourceExtension Source = "extension"
	SourceManual    Source = "manual"
)

// ErrNoAuth is returned when neither path is available. Providers
// should treat this as a non-error "waiting on browser" state.
var ErrNoAuth = errors.New("cookieaux: neither extension nor manual cookie available")

// DefaultTimeout is used when the ctx carries no deadline.
const DefaultTimeout = 20 * time.Second

// Fetcher makes HTTP requests on behalf of a cookie-gated provider.
// Construct one per Fetch call — it's a cheap value type.
type Fetcher struct {
	// Domain is the provider's canonical origin (e.g. "claude.ai").
	// Purely informational — used in MissingMessage hints.
	Domain string
	// ManualCookie is the raw Cookie header value from the user's
	// paste. Empty means no manual paste configured.
	ManualCookie string
}

// Available reports whether the fetcher can currently issue a request.
// Extension handshake OR manual paste = available.
func (f Fetcher) Available(ctx context.Context) bool {
	if strings.TrimSpace(f.ManualCookie) != "" {
		return true
	}
	return cookies.HostAvailable(ctx)
}

// Source picks which transport the next call will take.
func (f Fetcher) Source(ctx context.Context) Source {
	if cookies.HostAvailable(ctx) {
		return SourceExtension
	}
	if strings.TrimSpace(f.ManualCookie) != "" {
		return SourceManual
	}
	return SourceNone
}

// FetchJSON routes the request to the extension if it's up, else the
// manual-cookie httputil path, else returns ErrNoAuth.
func (f Fetcher) FetchJSON(ctx context.Context, url string, headers map[string]string, dst any) error {
	switch f.Source(ctx) {
	case SourceExtension:
		return cookies.FetchJSON(ctx, url, headers, dst)
	case SourceManual:
		return httputil.GetJSON(url, mergeManualHeaders(headers, f.ManualCookie), timeoutFromCtx(ctx), dst)
	default:
		return ErrNoAuth
	}
}

// FetchHTML is the HTML-body sibling of FetchJSON (used by Ollama's
// settings-page scrape).
func (f Fetcher) FetchHTML(ctx context.Context, url string, headers map[string]string) (string, error) {
	switch f.Source(ctx) {
	case SourceExtension:
		return cookies.FetchHTML(ctx, url, headers)
	case SourceManual:
		return httputil.GetHTML(url, mergeManualHeaders(headers, f.ManualCookie), timeoutFromCtx(ctx))
	default:
		return "", ErrNoAuth
	}
}

func mergeManualHeaders(extras map[string]string, cookie string) map[string]string {
	out := make(map[string]string, len(extras)+1)
	for k, v := range extras {
		out[k] = v
	}
	out["Cookie"] = cookie
	return out
}

func timeoutFromCtx(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining > 0 {
			return remaining
		}
	}
	return DefaultTimeout
}

// MissingMessage returns a user-facing explanation for Snapshot.Error
// when no transport is available. providerLabel is the site the user
// would sign into (e.g. "cursor.com").
func MissingMessage(providerLabel string) string {
	return fmt.Sprintf(
		"Install the Usage Buttons Chrome extension, or paste a Cookie header from %s in Plugin Settings.",
		providerLabel,
	)
}

// StaleMessage returns a recovery hint when a request came back
// 401/403 — the wording depends on which transport was used.
func StaleMessage(src Source, providerLabel string) string {
	if src == SourceExtension {
		return "Session from browser is stale. Sign in to " + providerLabel + " in Chrome, then refresh."
	}
	return "Cookie expired. Paste a fresh one from " + providerLabel + "."
}
