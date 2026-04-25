// Package kiro implements the Kiro usage provider.
//
// Auth: the Kiro CLI handles AWS Builder ID login.
// Command: kiro-cli chat --no-interactive "/usage"
package kiro

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

var (
	ansiRE       = regexp.MustCompile("\\x1b\\[[0-9;?]*[A-Za-z]|\\x1b\\].*?\\x07")
	legacyPlanRE = regexp.MustCompile(`(?m)\|\s*(KIRO\s+\w+)`)
	newPlanRE    = regexp.MustCompile(`(?m)Plan:\s*([^\r\n]+)`)
	resetRE      = regexp.MustCompile(`(?i)resets on (\d{2}/\d{2})`)
	percentRE    = regexp.MustCompile(`\x{2588}+\s*(\d+(?:\.\d+)?)%`)
	creditsRE    = regexp.MustCompile(`(?i)\((\d+(?:\.\d*)?)\s+of\s+(\d+(?:\.\d*)?)\s+covered`)
	bonusRE      = regexp.MustCompile(`(?i)Bonus credits:\s*(\d+(?:\.\d*)?)/(\d+(?:\.\d*)?)`)
	expiryRE     = regexp.MustCompile(`(?i)expires in (\d+) days?`)
)

// usageSnapshot is parsed from kiro-cli usage output.
type usageSnapshot struct {
	PlanName          string
	CreditsUsed       float64
	CreditsTotal      float64
	CreditsPercent    float64
	HasCredits        bool
	BonusCreditsUsed  *float64
	BonusCreditsTotal *float64
	BonusExpiryDays   *int
	ResetsAt          *time.Time
	UpdatedAt         time.Time
}

// Provider fetches Kiro usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "kiro" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kiro" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ff9900" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#111214" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Kiro CLI quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if err := ensureLoggedIn(); err != nil {
		return errorSnapshot(err.Error()), nil
	}
	output, err := usageOutput()
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	usage, err := parseOutput(output)
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// ensureLoggedIn validates that kiro-cli is installed and authenticated.
func ensureLoggedIn() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := providerutil.RunCommand(ctx, "kiro-cli", "whoami")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("kiro-cli whoami timed out")
		}
		return errors.New("kiro-cli not found. Install it from https://kiro.dev")
	}
	combined := strings.TrimSpace(result.Stdout)
	if strings.TrimSpace(result.Stderr) != "" {
		combined = strings.TrimSpace(result.Stderr)
	}
	lowered := strings.ToLower(combined)
	if strings.Contains(lowered, "not logged in") || strings.Contains(lowered, "login required") {
		return errors.New("Not logged in to Kiro. Run `kiro-cli login` first.")
	}
	if result.ExitCode != 0 {
		if combined == "" {
			return fmt.Errorf("Kiro CLI failed with status %d.", result.ExitCode)
		}
		return errors.New(combined)
	}
	if combined == "" {
		return errors.New("Kiro CLI whoami returned no output.")
	}
	return nil
}

// usageOutput runs the Kiro usage command and returns its output.
func usageOutput() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := providerutil.RunCommand(ctx, "kiro-cli", "chat", "--no-interactive", "/usage")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", errors.New("Kiro CLI timed out.")
		}
		return "", err
	}
	stdout := strings.TrimSpace(result.Stdout)
	stderr := strings.TrimSpace(result.Stderr)
	output := stdout
	if output == "" {
		output = stderr
	}
	lowered := strings.ToLower(stripANSI(output))
	if strings.Contains(lowered, "not logged in") ||
		strings.Contains(lowered, "login required") ||
		strings.Contains(lowered, "failed to initialize auth portal") ||
		strings.Contains(lowered, "kiro-cli login") ||
		strings.Contains(lowered, "oauth error") {
		return "", errors.New("Not logged in to Kiro. Run `kiro-cli login` first.")
	}
	if output == "" && result.ExitCode != 0 {
		return "", fmt.Errorf("Kiro CLI failed with status %d.", result.ExitCode)
	}
	if output == "" {
		return "", errors.New("Kiro CLI returned no usage output.")
	}
	return output, nil
}

// parseOutput extracts Kiro credits, bonus credits, and plan identity.
func parseOutput(output string) (usageSnapshot, error) {
	stripped := stripANSI(output)
	if strings.TrimSpace(stripped) == "" {
		return usageSnapshot{}, errors.New("Failed to parse Kiro usage: empty output from kiro-cli.")
	}
	lowered := strings.ToLower(stripped)
	if strings.Contains(lowered, "could not retrieve usage information") {
		return usageSnapshot{}, errors.New("Failed to parse Kiro usage: CLI could not retrieve usage information.")
	}
	if strings.Contains(lowered, "not logged in") ||
		strings.Contains(lowered, "login required") ||
		strings.Contains(lowered, "failed to initialize auth portal") ||
		strings.Contains(lowered, "kiro-cli login") ||
		strings.Contains(lowered, "oauth error") {
		return usageSnapshot{}, errors.New("Not logged in to Kiro. Run `kiro-cli login` first.")
	}

	usage := usageSnapshot{
		PlanName:       "Kiro",
		CreditsTotal:   50,
		CreditsUsed:    0,
		CreditsPercent: 0,
		UpdatedAt:      time.Now().UTC(),
	}
	if match := legacyPlanRE.FindStringSubmatch(stripped); len(match) >= 2 {
		usage.PlanName = strings.TrimSpace(match[1])
	}
	matchedNewFormat := false
	if match := newPlanRE.FindStringSubmatch(stripped); len(match) >= 2 {
		usage.PlanName = strings.TrimSpace(match[1])
		matchedNewFormat = true
	}

	matchedPercent := false
	if match := percentRE.FindStringSubmatch(stripped); len(match) >= 2 {
		if v, err := strconv.ParseFloat(match[1], 64); err == nil {
			usage.CreditsPercent = math.Max(0, math.Min(100, v))
			matchedPercent = true
		}
	}
	matchedCredits := false
	if match := creditsRE.FindStringSubmatch(stripped); len(match) >= 3 {
		if used, err := strconv.ParseFloat(match[1], 64); err == nil {
			usage.CreditsUsed = math.Max(0, used)
			matchedCredits = true
		}
		if total, err := strconv.ParseFloat(match[2], 64); err == nil {
			usage.CreditsTotal = math.Max(0, total)
			matchedCredits = true
		}
	}
	if matchedCredits && !matchedPercent && usage.CreditsTotal > 0 {
		usage.CreditsPercent = usage.CreditsUsed / usage.CreditsTotal * 100
	}
	if matchedPercent && !matchedCredits && usage.CreditsTotal > 0 {
		usage.CreditsUsed = usage.CreditsPercent / 100 * usage.CreditsTotal
	}
	if reset := resetDate(stripped); reset != nil {
		usage.ResetsAt = reset
	}
	if used, total := parseBonus(stripped); total != nil {
		usage.BonusCreditsUsed = used
		usage.BonusCreditsTotal = total
	}
	if days := parseExpiryDays(stripped); days != nil {
		usage.BonusExpiryDays = days
	}

	isManagedPlan := strings.Contains(lowered, "managed by admin") || strings.Contains(lowered, "managed by organization")
	if matchedNewFormat && isManagedPlan && !matchedPercent && !matchedCredits {
		return usage, nil
	}
	if !matchedPercent && !matchedCredits {
		return usageSnapshot{}, errors.New("Failed to parse Kiro usage: no recognizable usage patterns found.")
	}
	usage.HasCredits = true
	return usage, nil
}

// resetDate extracts a Kiro monthly reset date.
func resetDate(text string) *time.Time {
	match := resetRE.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	parts := strings.Split(match[1], "/")
	if len(parts) != 2 {
		return nil
	}
	month, monthErr := strconv.Atoi(parts[0])
	day, dayErr := strconv.Atoi(parts[1])
	if monthErr != nil || dayErr != nil {
		return nil
	}
	now := time.Now()
	reset := time.Date(now.Year(), time.Month(month), day, 0, 0, 0, 0, now.Location())
	if reset.After(now) {
		return &reset
	}
	reset = time.Date(now.Year()+1, time.Month(month), day, 0, 0, 0, 0, now.Location())
	return &reset
}

// parseBonus extracts the bonus credit lane.
func parseBonus(text string) (*float64, *float64) {
	match := bonusRE.FindStringSubmatch(text)
	if len(match) < 3 {
		return nil, nil
	}
	used, usedErr := strconv.ParseFloat(match[1], 64)
	total, totalErr := strconv.ParseFloat(match[2], 64)
	if usedErr != nil || totalErr != nil {
		return nil, nil
	}
	return floatPtr(math.Max(0, used)), floatPtr(math.Max(0, total))
}

// parseExpiryDays extracts the bonus credit expiry day count.
func parseExpiryDays(text string) *int {
	match := expiryRE.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	days, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &days
}

// snapshotFromUsage maps parsed Kiro output to provider metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	if usage.HasCredits {
		usedPct := math.Max(0, math.Min(100, usage.CreditsPercent))
		caption := ""
		if usage.CreditsTotal > 0 {
			caption = fmt.Sprintf("%s/%s credits", number(usage.CreditsUsed), number(usage.CreditsTotal))
		}
		m := providerutil.PercentRemainingMetric(
			"session-percent",
			"CREDITS",
			"Kiro monthly credits remaining",
			usedPct,
			usage.ResetsAt,
			caption,
			now,
		)
		if usage.CreditsTotal > 0 {
			remaining := int(math.Round(math.Max(0, usage.CreditsTotal-usage.CreditsUsed)))
			total := int(math.Round(usage.CreditsTotal))
			m.RawCount = &remaining
			m.RawMax = &total
		}
		metrics = append(metrics, m)
	}
	if usage.BonusCreditsUsed != nil && usage.BonusCreditsTotal != nil && *usage.BonusCreditsTotal > 0 {
		usedPct := math.Max(0, math.Min(100, *usage.BonusCreditsUsed / *usage.BonusCreditsTotal * 100))
		var resetAt *time.Time
		caption := ""
		if usage.BonusExpiryDays != nil {
			expires := time.Now().Add(time.Duration(*usage.BonusExpiryDays) * 24 * time.Hour)
			resetAt = &expires
			caption = fmt.Sprintf("expires in %dd", *usage.BonusExpiryDays)
		}
		m := providerutil.PercentRemainingMetric(
			"weekly-percent",
			"BONUS",
			"Kiro bonus credits remaining",
			usedPct,
			resetAt,
			caption,
			now,
		)
		remaining := int(math.Round(math.Max(0, *usage.BonusCreditsTotal-*usage.BonusCreditsUsed)))
		total := int(math.Round(*usage.BonusCreditsTotal))
		m.RawCount = &remaining
		m.RawMax = &total
		metrics = append(metrics, m)
	}
	if len(metrics) == 0 {
		return providers.Snapshot{
			ProviderID:   "kiro",
			ProviderName: providerName(usage.PlanName),
			Source:       "cli",
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Kiro plan does not expose usage metrics.",
		}
	}
	return providers.Snapshot{
		ProviderID:   "kiro",
		ProviderName: providerName(usage.PlanName),
		Source:       "cli",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// errorSnapshot returns a Kiro setup/error snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "kiro",
		ProviderName: "Kiro",
		Source:       "cli",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// stripANSI removes ANSI escape sequences from CLI output.
func stripANSI(text string) string {
	return ansiRE.ReplaceAllString(text, "")
}

// providerName combines Kiro with the parsed plan name.
func providerName(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" || strings.EqualFold(plan, "kiro") {
		return "Kiro"
	}
	return "Kiro " + plan
}

// number formats Kiro credit counts.
func number(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f", value)
	}
	return fmt.Sprintf("%.2f", value)
}

// floatPtr returns a pointer to v.
func floatPtr(v float64) *float64 {
	return &v
}

// init registers the Kiro provider with the package registry.
func init() {
	providers.Register(Provider{})
}
