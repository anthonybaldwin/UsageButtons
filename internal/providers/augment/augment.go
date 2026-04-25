// Package augment implements the Augment usage provider.
//
// Auth: `auggie account status` first, then the Usage Buttons Helper
// extension with the user's app.augmentcode.com browser session.
package augment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/cookieaux"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	creditsURL      = "https://app.augmentcode.com/api/credits"
	subscriptionURL = "https://app.augmentcode.com/api/subscription"
)

var (
	errCLIFallback = errors.New("augment cli fallback")
	maxPlanRE      = regexp.MustCompile(`(?i)([\d,]+)\s+credits\s*/\s*month`)
	remainingRE    = regexp.MustCompile(`(?i)([\d,]+)\s+remaining`)
	usedTotalRE    = regexp.MustCompile(`(?i)([\d,]+)\s*/\s*([\d,]+)\s+credits used`)
	cycleEndRE     = regexp.MustCompile(`(?i)ends\s+([\d/]+)`)
)

// creditsResponse is returned by /api/credits.
type creditsResponse struct {
	UsageUnitsRemaining                *float64 `json:"usageUnitsRemaining"`
	UsageUnitsConsumedThisBillingCycle *float64 `json:"usageUnitsConsumedThisBillingCycle"`
	UsageUnitsAvailable                *float64 `json:"usageUnitsAvailable"`
	UsageBalanceStatus                 string   `json:"usageBalanceStatus"`
}

// subscriptionResponse is returned by /api/subscription.
type subscriptionResponse struct {
	PlanName         string `json:"planName"`
	BillingPeriodEnd string `json:"billingPeriodEnd"`
	Email            string `json:"email"`
	Organization     string `json:"organization"`
}

// usageSnapshot is the normalized Augment quota state.
type usageSnapshot struct {
	CreditsRemaining *float64
	CreditsUsed      *float64
	CreditsLimit     *float64
	BillingCycleEnd  *time.Time
	AccountPlan      string
	AccountEmail     string
	UpdatedAt        time.Time
}

// cliFallbackError marks CLI failures that should fall through to browser auth.
type cliFallbackError struct {
	message string
}

// Error returns the fallback message.
func (e cliFallbackError) Error() string { return e.message }

// Unwrap exposes the fallback sentinel.
func (e cliFallbackError) Unwrap() error { return errCLIFallback }

// Provider fetches Augment usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "augment" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Augment" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#6366f1" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#10112f" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest Augment quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	usage, err := fetchCLI()
	if err == nil {
		return snapshotFromUsage(usage, "cli"), nil
	}
	if !errors.Is(err, errCLIFallback) {
		return errorSnapshot(err.Error(), "cli"), nil
	}

	webUsage, webErr := fetchWeb()
	if webErr == nil {
		return snapshotFromUsage(webUsage, "cookie"), nil
	}
	return errorSnapshot(webErr.Error(), "cookie"), nil
}

// fetchCLI runs `auggie account status` and parses the output.
func fetchCLI() (usageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := providerutil.RunCommand(ctx, "auggie", "account", "status")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return usageSnapshot{}, fmt.Errorf("Auggie CLI timed out.")
		}
		return usageSnapshot{}, cliFallbackError{"Auggie CLI not found."}
	}
	output := strings.TrimSpace(result.Stdout)
	stderr := strings.TrimSpace(result.Stderr)
	if output == "" {
		if stderr != "" {
			return usageSnapshot{}, cliFallbackError{stderr}
		}
		return usageSnapshot{}, cliFallbackError{"Auggie CLI returned no output."}
	}
	if strings.Contains(output, "Authentication failed") || strings.Contains(output, "auggie login") {
		return usageSnapshot{}, cliFallbackError{"Not authenticated. Run `auggie login` to authenticate."}
	}
	return parseCLIOutput(output)
}

// parseCLIOutput extracts credits from `auggie account status`.
func parseCLIOutput(output string) (usageSnapshot, error) {
	var remaining, used, total *float64
	var plan string
	var cycleEnd *time.Time
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if match := maxPlanRE.FindStringSubmatch(line); len(match) >= 2 {
			if n, ok := parseNumber(match[1]); ok {
				plan = fmt.Sprintf("%s credits/month", formatInt(n))
			}
		}
		if match := remainingRE.FindStringSubmatch(line); len(match) >= 2 {
			if n, ok := parseNumber(match[1]); ok {
				remaining = floatPtr(n)
			}
		}
		if match := usedTotalRE.FindStringSubmatch(line); len(match) >= 3 {
			if n, ok := parseNumber(match[1]); ok {
				used = floatPtr(n)
			}
			if n, ok := parseNumber(match[2]); ok {
				total = floatPtr(n)
			}
		}
		if match := cycleEndRE.FindStringSubmatch(line); len(match) >= 2 {
			cycleEnd = parseCycleEnd(match[1])
		}
	}
	if remaining == nil || used == nil || total == nil {
		return usageSnapshot{}, fmt.Errorf("Failed to parse auggie output: could not extract credits.")
	}
	return usageSnapshot{
		CreditsRemaining: remaining,
		CreditsUsed:      used,
		CreditsLimit:     total,
		BillingCycleEnd:  cycleEnd,
		AccountPlan:      plan,
		UpdatedAt:        time.Now().UTC(),
	}, nil
}

// fetchWeb fetches Augment usage through the browser Helper extension.
func fetchWeb() (usageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return usageSnapshot{}, errors.New(cookieaux.MissingMessage("app.augmentcode.com"))
	}

	var credits creditsResponse
	err := cookies.FetchJSON(ctx, creditsURL, nil, &credits)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return usageSnapshot{}, errors.New(cookieaux.StaleMessage("app.augmentcode.com"))
		}
		return usageSnapshot{}, err
	}

	var subscription subscriptionResponse
	_ = cookies.FetchJSON(ctx, subscriptionURL, nil, &subscription)
	limit := creditsLimit(&credits)
	var cycleEnd *time.Time
	if subscription.BillingPeriodEnd != "" {
		cycleEnd = parseISOTime(subscription.BillingPeriodEnd)
	}
	return usageSnapshot{
		CreditsRemaining: credits.UsageUnitsRemaining,
		CreditsUsed:      credits.UsageUnitsConsumedThisBillingCycle,
		CreditsLimit:     limit,
		BillingCycleEnd:  cycleEnd,
		AccountPlan:      strings.TrimSpace(subscription.PlanName),
		AccountEmail:     strings.TrimSpace(subscription.Email),
		UpdatedAt:        time.Now().UTC(),
	}, nil
}

// creditsLimit derives the Augment billing-cycle limit.
func creditsLimit(credits *creditsResponse) *float64 {
	if credits == nil {
		return nil
	}
	if credits.UsageUnitsRemaining != nil && credits.UsageUnitsConsumedThisBillingCycle != nil {
		return floatPtr(math.Max(0, *credits.UsageUnitsRemaining+*credits.UsageUnitsConsumedThisBillingCycle))
	}
	if credits.UsageUnitsAvailable != nil {
		return floatPtr(math.Max(0, *credits.UsageUnitsAvailable))
	}
	return nil
}

// snapshotFromUsage maps Augment usage to provider metrics.
func snapshotFromUsage(usage usageSnapshot, source string) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	providerName := "Augment"
	if usage.AccountPlan != "" {
		providerName += " " + usage.AccountPlan
	}
	if usage.CreditsLimit == nil || *usage.CreditsLimit <= 0 {
		return providers.Snapshot{
			ProviderID:   "augment",
			ProviderName: providerName,
			Source:       source,
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Augment response missing credit limit.",
		}
	}
	used := 0.0
	if usage.CreditsUsed != nil {
		used = math.Max(0, *usage.CreditsUsed)
	} else if usage.CreditsRemaining != nil {
		used = math.Max(0, *usage.CreditsLimit-*usage.CreditsRemaining)
	}
	caption := fmt.Sprintf("%s/%s credits", formatInt(used), formatInt(*usage.CreditsLimit))
	m := providerutil.PercentRemainingMetric(
		"session-percent",
		"CREDITS",
		"Augment credits remaining",
		used/(*usage.CreditsLimit)*100,
		usage.BillingCycleEnd,
		caption,
		now,
	)
	remaining := int(math.Round(math.Max(0, *usage.CreditsLimit-used)))
	total := int(math.Round(*usage.CreditsLimit))
	m.RawCount = &remaining
	m.RawMax = &total
	return providers.Snapshot{
		ProviderID:   "augment",
		ProviderName: providerName,
		Source:       source,
		Metrics:      []providers.MetricValue{m},
		Status:       "operational",
	}
}

// errorSnapshot returns an Augment setup or auth failure snapshot.
func errorSnapshot(message string, source string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "augment",
		ProviderName: "Augment",
		Source:       source,
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// parseNumber parses comma-grouped integer-ish credit values.
func parseNumber(raw string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(raw), ",", ""), 64)
	return n, err == nil
}

// parseCycleEnd parses Auggie billing-cycle dates like 1/8/2026.
func parseCycleEnd(raw string) *time.Time {
	t, err := time.ParseInLocation("1/2/2006", strings.TrimSpace(raw), time.Local)
	if err != nil {
		return nil
	}
	return &t
}

// parseISOTime parses subscription billing-period timestamps.
func parseISOTime(raw string) *time.Time {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
		return &t
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		return &t
	}
	return nil
}

// formatInt formats credit counts without decimals.
func formatInt(value float64) string {
	return strconv.FormatInt(int64(math.Round(value)), 10)
}

// floatPtr returns a pointer to v.
func floatPtr(v float64) *float64 {
	return &v
}

// init registers the Augment provider with the package registry.
func init() {
	providers.Register(Provider{})
}
