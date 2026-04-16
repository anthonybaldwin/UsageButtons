// Package cookieaux resolves a provider request's Cookie + User-Agent
// from either a manually pasted value in settings OR the companion
// Chrome extension, with a strict gating rule: when neither source
// has cookies and the extension is not yet handshaken, callers MUST
// NOT fire a request.
//
// The rule exists because firing cookie-missing requests at
// Cloudflare-protected providers (Claude web extras, Cursor, Ollama)
// produces repeat 403s that look like abuse and waste the user's
// request budget. A cold-start machine (Stream Deck launched before
// Chrome) should stay in a quiet "waiting on browser" state until
// the extension says hello.
package cookieaux

import (
	"context"
	"fmt"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
)

// Resolution carries the inputs a provider needs for a cookie-gated
// request: the Cookie header string and the User-Agent the extension
// reported for the originating browser (for UA-bound cf_clearance).
type Resolution struct {
	Header    string
	UserAgent string
	Source    string // "manual" | "extension"
}

// DefaultTimeout bounds the extension round trip.
const DefaultTimeout = 3 * time.Second

// Resolve tries manual paste first, falling back to the extension only
// if cookies.HostAvailable reports the extension has handshaken this
// session. Returns ok=false when neither source is available; callers
// MUST return a "waiting on browser" snapshot rather than fire a
// request in that state.
func Resolve(ctx context.Context, domain, manualHeader string) (Resolution, bool) {
	if manualHeader != "" {
		return Resolution{Header: manualHeader, Source: "manual"}, true
	}
	if !cookies.HostAvailable(ctx) {
		return Resolution{}, false
	}
	h, ua, err := cookies.Header(ctx, cookies.Query{Domain: domain})
	if err != nil {
		return Resolution{}, false
	}
	return Resolution{Header: h, UserAgent: ua, Source: "extension"}, true
}

// ResolveWithDeadline wraps Resolve with DefaultTimeout.
func ResolveWithDeadline(domain, manualHeader string) (Resolution, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	return Resolve(ctx, domain, manualHeader)
}

// Headers builds the http header map a provider should pass to
// httputil helpers. Extras is merged on top (wins) so callers can add
// Accept, Referer, Origin, etc.
func (r Resolution) Headers(extras map[string]string) map[string]string {
	out := make(map[string]string, 2+len(extras))
	out["Cookie"] = r.Header
	if r.UserAgent != "" {
		out["User-Agent"] = r.UserAgent
	}
	for k, v := range extras {
		out[k] = v
	}
	return out
}

// MissingMessage returns a user-facing explanation for Snapshot.Error
// when no cookie is resolvable. providerLabel is something like
// "cursor.com" — where the user would paste a cookie manually.
func MissingMessage(providerLabel string) string {
	return fmt.Sprintf(
		"Install the Usage Buttons Chrome extension, or paste a Cookie header from %s in Plugin Settings.",
		providerLabel,
	)
}
