// Package ollama implements the Ollama usage provider.
//
// Auth: Browser cookie pasted from ollama.com DevTools.
// Endpoint: GET https://ollama.com/settings (HTML scrape).
package ollama

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const settingsURL = "https://ollama.com/settings"

// Regex patterns ported from CodexBar's OllamaUsageFetcher.
var (
	planRe       = regexp.MustCompile(`(?i)Cloud Usage\s*</span>\s*<span[^>]*>([^<]+)</span>`)
	percentRe    = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)
	percentCSSRe = regexp.MustCompile(`(?i)width:\s*([0-9]+(?:\.[0-9]+)?)%`)
	resetTimeRe  = regexp.MustCompile(`data-time="([^"]+)"`)
)

// --- Provider implementation ---

// Provider fetches Ollama cloud usage data.
type Provider struct{}

func (Provider) ID() string         { return "ollama" }
func (Provider) Name() string       { return "Ollama" }
func (Provider) BrandColor() string { return "#888888" }
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "session-pace", "weekly-percent", "weekly-pace"}
}

func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	os := settings.OllamaSettings()
	if os.CookieHeader == "" {
		return providers.Snapshot{
			ProviderID:   "ollama",
			ProviderName: "Ollama",
			Source:       "none",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Paste a Cookie header from ollama.com in Plugin Settings.",
		}, nil
	}

	html, err := httputil.GetHTML(settingsURL, map[string]string{
		"Cookie":  os.CookieHeader,
		"Referer": "https://ollama.com/settings",
		"Origin":  "https://ollama.com",
	}, 15*time.Second)

	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return providers.Snapshot{
				ProviderID:   "ollama",
				ProviderName: "Ollama",
				Source:       "cookie",
				Metrics:      []providers.MetricValue{},
				Status:       "unknown",
				Error:        "Ollama cookie expired. Paste a fresh one from ollama.com.",
			}, nil
		}
		return providers.Snapshot{}, err
	}

	if isSignedOut(html) {
		return providers.Snapshot{
			ProviderID:   "ollama",
			ProviderName: "Ollama",
			Source:       "cookie",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Ollama cookie expired. Paste a fresh one from ollama.com.",
		}, nil
	}

	var metrics []providers.MetricValue
	now := time.Now().UTC().Format(time.RFC3339)

	// Session / Hourly usage
	sessionPct, sessionReset := parseUsageWindow(html, []string{"Session usage", "Hourly usage"})
	if sessionPct != nil {
		remaining := math.Max(0, 100-*sessionPct)
		ratio := remaining / 100
		m := providers.MetricValue{
			ID:           "session-percent",
			Label:        "SESSION",
			Name:         "Session usage remaining",
			Value:        math.Round(remaining),
			NumericValue: &remaining,
			NumericUnit:  "percent",
			Unit:         "%",
			Ratio:        &ratio,
			Direction:    "up",
			UpdatedAt:    now,
		}
		if sessionReset != nil {
			delta := time.Until(*sessionReset).Seconds()
			if delta < 0 {
				delta = 0
			}
			m.ResetInSeconds = &delta
		}
		metrics = append(metrics, m)
	}

	if sessionPct != nil && sessionReset != nil {
		if p := providers.PaceMetric(providers.PaceInput{
			MetricID: "session-pace", Label: "S.PACE", Name: "Session pace",
			UsedPercent: *sessionPct, WindowDuration: 5 * time.Hour, ResetIn: time.Until(*sessionReset),
		}); p != nil {
			metrics = append(metrics, *p)
		}
	}

	// Weekly usage
	weeklyPct, weeklyReset := parseUsageWindow(html, []string{"Weekly usage"})
	if weeklyPct != nil {
		remaining := math.Max(0, 100-*weeklyPct)
		ratio := remaining / 100
		m := providers.MetricValue{
			ID:           "weekly-percent",
			Label:        "WEEKLY",
			Name:         "Weekly usage remaining",
			Value:        math.Round(remaining),
			NumericValue: &remaining,
			NumericUnit:  "percent",
			Unit:         "%",
			Ratio:        &ratio,
			Direction:    "up",
			UpdatedAt:    now,
		}
		if weeklyReset != nil {
			delta := time.Until(*weeklyReset).Seconds()
			if delta < 0 {
				delta = 0
			}
			m.ResetInSeconds = &delta
		}
		metrics = append(metrics, m)
	}

	if weeklyPct != nil && weeklyReset != nil {
		if p := providers.PaceMetric(providers.PaceInput{
			MetricID: "weekly-pace", Label: "W.PACE", Name: "Weekly pace",
			UsedPercent: *weeklyPct, WindowDuration: 7 * 24 * time.Hour, ResetIn: time.Until(*weeklyReset),
		}); p != nil {
			metrics = append(metrics, *p)
		}
	}

	planLabel := "Ollama"
	if m := planRe.FindStringSubmatch(html); len(m) > 1 {
		plan := strings.TrimSpace(m[1])
		if plan != "" {
			planLabel = "Ollama " + plan
		}
	}

	return providers.Snapshot{
		ProviderID:   "ollama",
		ProviderName: planLabel,
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}, nil
}

// parseUsageWindow looks for one of the given labels in the HTML, then
// extracts the percentage and optional reset timestamp from the next
// 800 characters after the label.
func parseUsageWindow(html string, labels []string) (*float64, *time.Time) {
	lower := strings.ToLower(html)
	var window string
	for _, label := range labels {
		idx := strings.Index(lower, strings.ToLower(label))
		if idx < 0 {
			continue
		}
		end := idx + len(label) + 800
		if end > len(html) {
			end = len(html)
		}
		window = html[idx:end]
		break
	}
	if window == "" {
		return nil, nil
	}

	pct := extractPercent(window)
	if pct == nil {
		return nil, nil
	}

	var resetAt *time.Time
	if m := resetTimeRe.FindStringSubmatch(window); len(m) > 1 {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			resetAt = &t
		} else if t, err := time.Parse("2006-01-02T15:04:05Z", m[1]); err == nil {
			resetAt = &t
		}
	}

	return pct, resetAt
}

// extractPercent tries two patterns: "N% used" then CSS "width: N%".
func extractPercent(s string) *float64 {
	if m := percentRe.FindStringSubmatch(s); len(m) > 1 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return &v
		}
	}
	if m := percentCSSRe.FindStringSubmatch(s); len(m) > 1 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return &v
		}
	}
	return nil
}

// isSignedOut detects if the HTML is a login page rather than settings.
func isSignedOut(html string) bool {
	lower := strings.ToLower(html)
	hasForm := strings.Contains(lower, "<form")
	if !hasForm {
		return false
	}

	hasSignIn := strings.Contains(lower, "sign in to ollama") ||
		strings.Contains(lower, "log in to ollama")
	hasAuthRoute := strings.Contains(lower, "/api/auth/signin") ||
		strings.Contains(lower, "/auth/signin") ||
		strings.Contains(lower, `action="/login"`) ||
		strings.Contains(lower, `action='/login'`) ||
		strings.Contains(lower, `action="/signin"`) ||
		strings.Contains(lower, `action='/signin'`)
	hasPassword := strings.Contains(lower, `type="password"`) ||
		strings.Contains(lower, `type='password'`)
	hasEmail := strings.Contains(lower, `type="email"`) ||
		strings.Contains(lower, `type='email'`)

	if hasSignIn && (hasAuthRoute || hasPassword || hasEmail) {
		return true
	}
	if hasAuthRoute && hasPassword && hasEmail {
		return true
	}
	return false
}

func init() {
	providers.Register(Provider{})
}
