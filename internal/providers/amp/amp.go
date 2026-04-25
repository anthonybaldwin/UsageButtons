// Package amp implements the Amp usage provider.
//
// Auth: Usage Buttons Helper extension with the user's ampcode.com browser
// session. Endpoint: GET https://ampcode.com/settings.
package amp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const settingsURL = "https://ampcode.com/settings"

// freeTierUsage is Amp Free usage embedded in the settings page.
type freeTierUsage struct {
	Quota               float64
	Used                float64
	HourlyReplenishment float64
	WindowHours         *float64
}

// Provider fetches Amp usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "amp" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Amp" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#dc2626" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#250b0b" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest Amp Free usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("ampcode.com")), nil
	}

	html, err := cookies.FetchHTML(ctx, settingsURL, map[string]string{
		"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Referer":    settingsURL,
		"Origin":     "https://ampcode.com",
		"User-Agent": httputil.DefaultUserAgent,
	})
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("ampcode.com")), nil
		}
		return providers.Snapshot{}, err
	}
	usage, err := parseHTML(html)
	if err != nil {
		if looksSignedOut(html) {
			return errorSnapshot(cookieaux.StaleMessage("ampcode.com")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// parseHTML extracts Amp's embedded free-tier payload.
func parseHTML(html string) (freeTierUsage, error) {
	for _, token := range []string{"freeTierUsage", "getFreeTierUsage"} {
		object, ok := extractObject(token, html)
		if !ok {
			continue
		}
		usage, ok := parseUsageObject(object)
		if ok {
			return usage, nil
		}
	}
	return freeTierUsage{}, fmt.Errorf("could not parse Amp usage: missing Amp Free usage data")
}

// extractObject returns the first balanced object after token.
func extractObject(token string, text string) (string, bool) {
	tokenIndex := strings.Index(text, token)
	if tokenIndex < 0 {
		return "", false
	}
	braceOffset := strings.Index(text[tokenIndex+len(token):], "{")
	if braceOffset < 0 {
		return "", false
	}
	start := tokenIndex + len(token) + braceOffset
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1], true
			}
		}
	}
	return "", false
}

// parseUsageObject extracts numeric fields from one free-tier object.
func parseUsageObject(object string) (freeTierUsage, bool) {
	quota, okQuota := objectNumber("quota", object)
	used, okUsed := objectNumber("used", object)
	hourly, okHourly := objectNumber("hourlyReplenishment", object)
	if !okQuota || !okUsed || !okHourly {
		return freeTierUsage{}, false
	}
	usage := freeTierUsage{
		Quota:               quota,
		Used:                used,
		HourlyReplenishment: hourly,
	}
	if window, ok := objectNumber("windowHours", object); ok {
		usage.WindowHours = &window
	}
	return usage, true
}

// objectNumber finds a JavaScript object numeric literal by key.
func objectNumber(key string, object string) (float64, bool) {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
	match := re.FindStringSubmatch(object)
	if len(match) < 2 {
		return 0, false
	}
	return providerutil.FloatValue(match[1])
}

// looksSignedOut reports whether the fetched page looks like a login page.
func looksSignedOut(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "log in") ||
		strings.Contains(lower, "login") ||
		strings.Contains(lower, "/login")
}

// snapshotFromUsage maps Amp Free usage into Stream Deck metrics.
func snapshotFromUsage(usage freeTierUsage) providers.Snapshot {
	now := providerutil.NowString()
	quota := math.Max(0, usage.Quota)
	used := math.Max(0, usage.Used)
	usedPct := 0.0
	if quota > 0 {
		usedPct = math.Min(100, used/quota*100)
	}
	var resetsAt *time.Time
	if quota > 0 && usage.HourlyReplenishment > 0 {
		seconds := math.Max(0, used/usage.HourlyReplenishment*3600)
		reset := time.Now().Add(time.Duration(seconds) * time.Second)
		resetsAt = &reset
	}
	caption := fmt.Sprintf("%s/%s free", wholeNumber(used), wholeNumber(quota))
	if usage.WindowHours != nil && *usage.WindowHours > 0 {
		caption = fmt.Sprintf("%s, %.0fh window", caption, *usage.WindowHours)
	}
	m := providerutil.PercentRemainingMetric(
		"session-percent",
		"AMP FREE",
		"Amp Free remaining",
		usedPct,
		resetsAt,
		caption,
		now,
	)
	if quota > 0 {
		remaining := int(math.Round(math.Max(0, quota-used)))
		total := int(math.Round(quota))
		m.RawCount = &remaining
		m.RawMax = &total
	}
	return providers.Snapshot{
		ProviderID:   "amp",
		ProviderName: "Amp Free",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{m},
		Status:       "operational",
	}
}

// errorSnapshot returns an Amp setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "amp",
		ProviderName: "Amp",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// wholeNumber formats whole-number quota values.
func wholeNumber(value float64) string {
	return fmt.Sprintf("%.0f", math.Round(value))
}

// init registers the Amp provider with the package registry.
func init() {
	providers.Register(Provider{})
}
