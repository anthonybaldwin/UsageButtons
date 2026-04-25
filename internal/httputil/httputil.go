// Package httputil provides a thin HTTP+JSON helper for providers.
package httputil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent is sent on every request unless the caller
// explicitly sets a User-Agent header. Go's default "Go-http-client/1.1"
// gets blocked by Cloudflare, so we use a real browser UA.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Error is returned for non-2xx HTTP responses.
type Error struct {
	Status     int
	StatusText string
	Body       string
	URL        string
	Headers    http.Header
}

func (e *Error) Error() string {
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	return fmt.Sprintf("HTTP %d %s from %s: %s", e.Status, e.StatusText, e.URL, body)
}

// Header returns a response header value (case-insensitive).
func (e *Error) Header(name string) string {
	return e.Headers.Get(name)
}

// RetryAfter returns the absolute time the server asked us to wait
// until before retrying, parsed from common rate-limit / server-busy
// response headers. Returns zero-time when no recognized hint is
// present or the status code isn't one that should carry one.
//
// Recognized headers, in order of preference:
//   - Retry-After            (RFC 7231: seconds OR HTTP date)
//   - x-ratelimit-reset      (Unix timestamp OR seconds-from-now)
//   - anthropic-ratelimit-requests-reset (ISO 8601)
//   - anthropic-ratelimit-tokens-reset   (ISO 8601)
func (e *Error) RetryAfter() time.Time {
	if e == nil {
		return time.Time{}
	}
	if e.Status != 429 && e.Status != 503 {
		return time.Time{}
	}
	now := time.Now()
	if v := e.Headers.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return t
		}
	}
	if v := e.Headers.Get("x-ratelimit-reset"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			// Heuristic: large integers are absolute Unix timestamps;
			// small integers are a relative delay in seconds. Anything
			// far enough in the future to be reached as "now + secs"
			// (roughly past one year from epoch) is treated as absolute.
			if secs > now.Unix()-86400 {
				return time.Unix(secs, 0)
			}
			return now.Add(time.Duration(secs) * time.Second)
		}
	}
	for _, name := range []string{
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		if v := e.Headers.Get(name); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// GetJSON performs a GET request and decodes the JSON response into dst.
func GetJSON(url string, headers map[string]string, timeout time.Duration, dst any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("transport error %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body %s: %w", url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        url,
			Headers:    resp.Header,
		}
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("invalid JSON from %s: %w", url, err)
	}
	return nil
}

// PostJSON performs a POST request with a JSON body and decodes the response.
func PostJSON(url string, headers map[string]string, payload any, timeout time.Duration, dst any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("build request %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("transport error %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body %s: %w", url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        url,
			Headers:    resp.Header,
		}
	}

	if dst != nil {
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("invalid JSON from %s: %w", url, err)
		}
	}
	return nil
}

// Truncate shortens a string to n chars.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
