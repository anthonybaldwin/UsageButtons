package cookies

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HostName is the Chrome native-messaging host identifier. It must
// match the "name" field in the host manifest and the string extensions
// pass to chrome.runtime.connectNative.
const HostName = "io.github.anthonybaldwin.usagebuttons"

// Sentinel errors. Callers should prefer errors.Is over string compare.
var (
	// ErrHostUnavailable is returned when the native host is not
	// reachable — extension not installed, Chrome not running, or the
	// extension has not yet handshaken this session. Providers should
	// treat this as a quiet "waiting on browser" state, not an error
	// to surface to the user.
	ErrHostUnavailable = errors.New("cookies: native host unavailable")

	// ErrDomainNotAllowed is returned when a Query names a domain that
	// is not in the compile-time allowlist. This is an integrity check;
	// the extension enforces the same allowlist independently.
	ErrDomainNotAllowed = errors.New("cookies: domain not in allowlist")

	// ErrNoCookies is returned when a query matched zero cookies.
	ErrNoCookies = errors.New("cookies: no cookies found")
)

// Query selects which cookies to return. Domain is required and must
// match (or be a subdomain of) an entry in Allowed.
type Query struct {
	Domain string   // e.g. "claude.ai"
	Names  []string // optional — filter by cookie name; empty means all
}

// Validate rejects empty or non-allowlisted domains before we burn a
// round trip to the extension.
func (q Query) Validate() error {
	if strings.TrimSpace(q.Domain) == "" {
		return fmt.Errorf("cookies: query domain is required")
	}
	if !IsAllowed(q.Domain) {
		return fmt.Errorf("%w: %q", ErrDomainNotAllowed, q.Domain)
	}
	return nil
}

// Cookie is the subset of chrome.cookies.Cookie fields we use.
type Cookie struct {
	Domain  string
	Name    string
	Value   string
	Path    string
	Expires time.Time // zero value means session cookie
	Secure  bool
}

// Bundle groups cookies with the User-Agent string the extension
// reports for the originating browser. Callers should send requests
// to the provider using this UA, because cf_clearance is bound to the
// UA that obtained it.
type Bundle struct {
	Cookies   []Cookie
	UserAgent string
}

// Get asks the extension (via the native host) for cookies matching q.
// Returns ErrHostUnavailable if the host isn't reachable yet — callers
// should treat that as a quiet "waiting on browser" state.
func Get(ctx context.Context, q Query) (Bundle, error) {
	if err := q.Validate(); err != nil {
		return Bundle{}, err
	}
	return dispatchGet(ctx, q)
}

// Header is a convenience returning "name1=v1; name2=v2" suitable for
// a Cookie: request header, plus the UA reported by the extension.
func Header(ctx context.Context, q Query) (header, userAgent string, err error) {
	b, err := Get(ctx, q)
	if err != nil {
		return "", "", err
	}
	if len(b.Cookies) == 0 {
		return "", b.UserAgent, ErrNoCookies
	}
	var sb strings.Builder
	for i, c := range b.Cookies {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(c.Name)
		sb.WriteByte('=')
		sb.WriteString(c.Value)
	}
	return sb.String(), b.UserAgent, nil
}

// HostAvailable returns true only when the native host IPC endpoint is
// reachable AND the extension has handshaken this session. Cold-start
// (Stream Deck launched before Chrome) returns false. Providers must
// gate requests on this.
func HostAvailable(ctx context.Context) bool {
	return probeHost(ctx)
}

// dispatchGet is overridden in client_*.go once the IPC transport is
// wired up. Keeping it as a package var lets the skeleton compile and
// tests run before cmd/native-host exists.
var dispatchGet = func(ctx context.Context, q Query) (Bundle, error) {
	_ = ctx
	_ = q
	return Bundle{}, ErrHostUnavailable
}

// probeHost mirrors dispatchGet — overridden by the platform-specific
// client once IPC lands. Default behavior keeps cookie-gated providers
// in the "waiting on browser" state until the transport is real.
var probeHost = func(ctx context.Context) bool {
	_ = ctx
	return false
}
