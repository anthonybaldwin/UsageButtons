package cookies

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

// HostName is the native-messaging host identifier. It must match the
// "name" field in the host manifest and the string extensions pass to
// chrome.runtime.connectNative (or browser.runtime.connectNative in
// Firefox).
const HostName = "io.github.anthonybaldwin.usagebuttons"

// DefaultExtensionID is the deterministic Chrome extension ID derived
// from the pinned public key in chrome-extension/manifest.json. Chrome
// computes this ID from SHA-256(SubjectPublicKeyInfo), so the ID is
// stable across machines, reinstalls, and sideloads — which means the
// plugin can auto-register the native-messaging manifest without
// asking the user to paste an ID. The private key that corresponds to
// this public half is gitignored as chrome-extension-private.pem and
// is only needed for future Chrome Web Store uploads.
const DefaultExtensionID = "ggablblpfclemapimphpjdhlbhdombnm"

// Sentinel errors. Callers should prefer errors.Is over string compare.
var (
	// ErrHostUnavailable is returned when the native host is not
	// reachable — extension not installed, browser not running, or the
	// extension has not yet handshaken this session. Providers should
	// treat this as a quiet "waiting on browser" state.
	ErrHostUnavailable = errors.New("cookies: native host unavailable")

	// ErrOriginNotAllowed is returned when a Request targets a URL
	// whose host isn't covered by the compile-time allowlist. The
	// extension enforces the same allowlist independently.
	ErrOriginNotAllowed = errors.New("cookies: url origin not in allowlist")
)

// Request is what the plugin asks the extension to fetch on its
// behalf. The browser handles cookies and User-Agent automatically
// via credentials:"include"; do NOT set a Cookie header explicitly
// (browsers refuse it anyway).
type Request struct {
	URL     string
	Method  string // default "GET"
	Headers map[string]string
	Body    []byte // optional, for POST/PUT
}

// Response carries the extension's fetch result.
type Response struct {
	Status      int
	StatusText  string
	Body        []byte
	ContentType string
	// UserAgent is the browser's UA at fetch time — informational.
	UserAgent string
}

// Fetch dispatches a Request through the extension and returns the raw
// response. Non-2xx statuses return a *httputil.Error so providers can
// use the same errors.As(err, *httputil.Error) checks they use for the
// direct-HTTP fallback.
func Fetch(ctx context.Context, r Request) (Response, error) {
	if err := validateRequest(r); err != nil {
		return Response{}, err
	}
	return dispatchFetch(ctx, r)
}

// FetchJSON fetches URL via the extension and decodes the JSON body
// into dst. Headers is optional; Accept defaults to application/json.
func FetchJSON(ctx context.Context, url string, headers map[string]string, dst any) error {
	hh := cloneHeaders(headers)
	if _, ok := hh["Accept"]; !ok {
		hh["Accept"] = "application/json"
	}
	resp, err := Fetch(ctx, Request{URL: url, Method: "GET", Headers: hh})
	if err != nil {
		return err
	}
	if err := statusError(url, resp); err != nil {
		return err
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("cookies: empty body from %s", url)
	}
	if err := json.Unmarshal(resp.Body, dst); err != nil {
		return fmt.Errorf("cookies: invalid JSON from %s: %w", url, err)
	}
	return nil
}

// FetchHTML fetches URL via the extension and returns the response
// body as a string. Headers is optional.
func FetchHTML(ctx context.Context, url string, headers map[string]string) (string, error) {
	hh := cloneHeaders(headers)
	if _, ok := hh["Accept"]; !ok {
		hh["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	}
	resp, err := Fetch(ctx, Request{URL: url, Method: "GET", Headers: hh})
	if err != nil {
		return "", err
	}
	if err := statusError(url, resp); err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// HostAvailable returns true only when the native host IPC endpoint
// is reachable AND the extension has handshaken this session. Gate
// cookie-gated provider requests on this.
func HostAvailable(ctx context.Context) bool {
	return probeHost(ctx)
}

func validateRequest(r Request) error {
	if strings.TrimSpace(r.URL) == "" {
		return fmt.Errorf("cookies: request URL is required")
	}
	if !URLAllowed(r.URL) {
		return fmt.Errorf("%w: %q", ErrOriginNotAllowed, r.URL)
	}
	return nil
}

func statusError(url string, resp Response) error {
	if resp.Status >= 200 && resp.Status < 300 {
		return nil
	}
	return &httputil.Error{
		Status:     resp.Status,
		StatusText: resp.StatusText,
		Body:       string(resp.Body),
		URL:        url,
	}
}

func cloneHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// dispatchFetch is overridden in ipc.go init(); keeping it as a var
// lets unit tests substitute a fake and lets the skeleton compile
// before the transport is wired.
var dispatchFetch = func(ctx context.Context, r Request) (Response, error) {
	_ = ctx
	_ = r
	return Response{}, ErrHostUnavailable
}

var probeHost = func(ctx context.Context) bool {
	_ = ctx
	return false
}

// b64 helpers are exported so tests in sibling packages can build
// wire bytes that match the protocol without poking internals.
var b64 = base64.StdEncoding

// wire deadline for the extension fetch round trip. Matches the
// existing httputil 15–30s timeouts the providers use directly.
const defaultFetchTimeout = 20 * time.Second
