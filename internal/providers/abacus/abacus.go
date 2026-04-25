// Package abacus implements the Abacus AI usage provider.
//
// Auth: Usage Buttons Helper extension with the user's apps.abacus.ai
// browser session. Endpoints:
// GET /api/_getOrganizationComputePoints and POST /api/_getBillingInfo.
package abacus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	computePointsURL = "https://apps.abacus.ai/api/_getOrganizationComputePoints"
	billingInfoURL   = "https://apps.abacus.ai/api/_getBillingInfo"
)

// apiEnvelope is the common Abacus API response wrapper.
type apiEnvelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// computePointsResponse is the required Abacus credit payload.
type computePointsResponse struct {
	TotalComputePoints *float64 `json:"totalComputePoints"`
	ComputePointsLeft  *float64 `json:"computePointsLeft"`
}

// billingInfoResponse is the optional Abacus plan/reset payload.
type billingInfoResponse struct {
	NextBillingDate any    `json:"nextBillingDate"`
	CurrentTier     string `json:"currentTier"`
}

// usageSnapshot is the normalized Abacus credit state.
type usageSnapshot struct {
	CreditsUsed  float64
	CreditsTotal float64
	ResetsAt     *time.Time
	PlanName     string
	UpdatedAt    time.Time
}

// Provider fetches Abacus AI usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "abacus" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Abacus AI" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#38bdf8" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#082033" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest Abacus AI compute-credit snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("apps.abacus.ai")), nil
	}
	usage, err := fetchUsage(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("apps.abacus.ai")), nil
		}
		if looksAuthError(err.Error()) {
			return errorSnapshot(cookieaux.StaleMessage("apps.abacus.ai")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsage fetches compute points and optional billing details.
func fetchUsage(ctx context.Context) (usageSnapshot, error) {
	var points computePointsResponse
	if err := fetchResult(ctx, cookies.Request{
		URL:    computePointsURL,
		Method: "GET",
		Headers: map[string]string{
			"Accept":     "application/json",
			"Origin":     "https://apps.abacus.ai",
			"Referer":    "https://apps.abacus.ai/chatllm/admin/compute-points-usage",
			"User-Agent": httputil.DefaultUserAgent,
		},
	}, &points); err != nil {
		return usageSnapshot{}, err
	}
	if points.TotalComputePoints == nil || points.ComputePointsLeft == nil {
		return usageSnapshot{}, fmt.Errorf("Abacus AI response missing compute point fields")
	}

	var billing billingInfoResponse
	billingErr := fetchResult(ctx, cookies.Request{
		URL:    billingInfoURL,
		Method: "POST",
		Headers: map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
			"Origin":       "https://apps.abacus.ai",
			"Referer":      "https://apps.abacus.ai/chatllm/admin/compute-points-usage",
			"User-Agent":   httputil.DefaultUserAgent,
		},
		Body: []byte("{}"),
	}, &billing)

	total := math.Max(0, *points.TotalComputePoints)
	left := math.Max(0, *points.ComputePointsLeft)
	used := math.Max(0, total-left)
	var resetsAt *time.Time
	planName := ""
	if billingErr == nil {
		if t, ok := providerutil.TimeValue(billing.NextBillingDate); ok {
			resetsAt = &t
		}
		planName = strings.TrimSpace(billing.CurrentTier)
	}

	return usageSnapshot{
		CreditsUsed:  used,
		CreditsTotal: total,
		ResetsAt:     resetsAt,
		PlanName:     planName,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}

// fetchResult fetches one Abacus API endpoint and decodes its result object.
func fetchResult(ctx context.Context, req cookies.Request, dst any) error {
	resp, err := cookies.Fetch(ctx, req)
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        req.URL,
		}
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return fmt.Errorf("invalid Abacus AI JSON from %s: %w", req.URL, err)
	}
	if !envelope.Success {
		return fmt.Errorf("Abacus AI API error from %s: %s", req.URL, envelopeError(envelope.Error))
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("Abacus AI API response from %s has no result", req.URL)
	}
	if err := json.Unmarshal(envelope.Result, dst); err != nil {
		return fmt.Errorf("invalid Abacus AI result from %s: %w", req.URL, err)
	}
	return nil
}

// envelopeError formats an Abacus API error payload for display.
func envelopeError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "unknown error"
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		if msg := providerutil.FirstString(m, "message", "error", "detail"); msg != "" {
			return msg
		}
	}
	return string(raw)
}

// snapshotFromUsage maps Abacus compute credits into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	total := math.Max(0, usage.CreditsTotal)
	used := math.Max(0, usage.CreditsUsed)
	if total > 0 {
		used = math.Min(used, total)
	}
	usedPct := 0.0
	if total > 0 {
		usedPct = used / total * 100
	}
	caption := fmt.Sprintf("%s/%s credits", formatCredits(used), formatCredits(total))
	if usage.PlanName != "" {
		caption = usage.PlanName + " - " + caption
	}
	m := providerutil.PercentRemainingMetric(
		"session-percent",
		"CREDITS",
		"Abacus AI credits remaining",
		usedPct,
		usage.ResetsAt,
		caption,
		now,
	)
	if total > 0 {
		remaining := int(math.Round(math.Max(0, total-used)))
		maximum := int(math.Round(total))
		m.RawCount = &remaining
		m.RawMax = &maximum
	}
	return providers.Snapshot{
		ProviderID:   "abacus",
		ProviderName: providerName(usage.PlanName),
		Source:       "cookie",
		Metrics:      []providers.MetricValue{m},
		Status:       "operational",
	}
}

// providerName returns Abacus AI with the plan name when available.
func providerName(planName string) string {
	planName = strings.TrimSpace(planName)
	if planName == "" {
		return "Abacus AI"
	}
	return "Abacus AI " + planName
}

// errorSnapshot returns an Abacus AI setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "abacus",
		ProviderName: "Abacus AI",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// looksAuthError reports whether an application-level error means login is stale.
func looksAuthError(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "expired") ||
		strings.Contains(lower, "session") ||
		strings.Contains(lower, "login") ||
		strings.Contains(lower, "authenticate") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "forbidden")
}

// formatCredits formats compute credits compactly for button captions.
func formatCredits(value float64) string {
	if math.Abs(value) >= 1000 {
		return fmt.Sprintf("%.0f", math.Round(value))
	}
	if math.Abs(value-math.Round(value)) < 0.05 {
		return fmt.Sprintf("%.0f", math.Round(value))
	}
	return fmt.Sprintf("%.1f", value)
}

// init registers the Abacus AI provider with the package registry.
func init() {
	providers.Register(Provider{})
}
