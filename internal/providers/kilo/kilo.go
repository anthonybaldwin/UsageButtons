// Package kilo implements the Kilo usage provider.
//
// Auth: Property Inspector settings field or KILO_API_KEY environment variable,
// with fallback to the Kilo CLI auth file at ~/.local/share/kilo/auth.json.
// Endpoint: GET https://app.kilo.ai/api/trpc/{batch procedures}
package kilo

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
)

const (
	defaultTRPCBase = "https://app.kilo.ai/api/trpc"
)

var (
	errUnauthorized = errors.New("kilo unauthorized")
	kiloProcedures  = []string{
		"user.getCreditBlocks",
		"kiloPass.getState",
		"user.getAutoTopUpPaymentMethod",
	}
	optionalProcedures = map[string]bool{
		"user.getAutoTopUpPaymentMethod": true,
	}
)

// usageSnapshot is the parsed Kilo quota state.
type usageSnapshot struct {
	CreditsUsed      *float64
	CreditsTotal     *float64
	CreditsRemaining *float64
	PassUsed         *float64
	PassTotal        *float64
	PassRemaining    *float64
	PassBonus        *float64
	PassResetsAt     *time.Time
	PlanName         string
	AutoTopUpEnabled *bool
	AutoTopUpMethod  string
	UpdatedAt        time.Time
}

// passFields holds the Kilo Pass usage lane before it is merged into a snapshot.
type passFields struct {
	Used      *float64
	Total     *float64
	Remaining *float64
	Bonus     *float64
	ResetsAt  *time.Time
}

// Provider fetches Kilo usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "kilo" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "Kilo" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#f27027" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#21130a" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Kilo quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	if apiKey := getAPIKey(); apiKey != "" {
		usage, err := fetchUsage(apiKey)
		if err == nil {
			return snapshotFromUsage(usage, "api-key"), nil
		}
		if !errors.Is(err, errUnauthorized) {
			return providers.Snapshot{}, err
		}
		if cliToken, _, cliErr := cliAuthToken(); cliErr == nil {
			usage, cliFetchErr := fetchUsage(cliToken)
			if cliFetchErr == nil {
				return snapshotFromUsage(usage, "cli"), nil
			}
			if errors.Is(cliFetchErr, errUnauthorized) {
				return authErrorSnapshot("Kilo CLI session unauthorized. Run `kilo login` to refresh auth.json."), nil
			}
			return providers.Snapshot{}, cliFetchErr
		}
		return authErrorSnapshot("Kilo API key unauthorized. Check KILO_API_KEY or run `kilo login`."), nil
	}

	if cliToken, _, err := cliAuthToken(); err == nil {
		usage, fetchErr := fetchUsage(cliToken)
		if fetchErr == nil {
			return snapshotFromUsage(usage, "cli"), nil
		}
		if errors.Is(fetchErr, errUnauthorized) {
			return authErrorSnapshot("Kilo CLI session unauthorized. Run `kilo login` to refresh auth.json."), nil
		}
		return providers.Snapshot{}, fetchErr
	}

	return providerutil.MissingAuthSnapshot(
		"kilo",
		"Kilo",
		"Enter a Kilo API key in the Kilo tab, set KILO_API_KEY, or run `kilo login`.",
	), nil
}

// getAPIKey resolves a Kilo API key from user settings or env vars.
func getAPIKey() string {
	return settings.ResolveAPIKey(settings.ProviderKeysGet().KiloKey, "KILO_API_KEY")
}

// authErrorSnapshot returns an actionable Kilo auth failure snapshot.
func authErrorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "kilo",
		ProviderName: "Kilo",
		Source:       "auth",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// cliAuthToken reads the Kilo CLI auth token from auth.json.
func cliAuthToken() (token string, path string, err error) {
	path = cliAuthPath()
	if path == "" {
		return "", "", errors.New("home directory unavailable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", path, err
	}
	var payload struct {
		Kilo *struct {
			Access string `json:"access"`
		} `json:"kilo"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", path, err
	}
	if payload.Kilo == nil {
		return "", path, errors.New("missing kilo section")
	}
	token = cleanToken(payload.Kilo.Access)
	if token == "" {
		return "", path, errors.New("missing access token")
	}
	return token, path, nil
}

// cliAuthPath returns the documented Kilo CLI auth file path.
func cliAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "kilo", "auth.json")
}

// cleanToken trims whitespace and optional quote wrapping from credentials.
func cleanToken(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return strings.TrimSpace(v)
}

// fetchUsage calls Kilo's tRPC batch endpoint and parses the response.
func fetchUsage(apiKey string) (usageSnapshot, error) {
	batchURL, err := makeBatchURL()
	if err != nil {
		return usageSnapshot{}, err
	}
	var raw any
	err = httputil.GetJSON(batchURL, map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Accept":        "application/json",
	}, 15*time.Second, &raw)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return usageSnapshot{}, errUnauthorized
		}
		return usageSnapshot{}, err
	}
	return parseUsage(raw)
}

// makeBatchURL builds Kilo's tRPC batch URL.
func makeBatchURL() (string, error) {
	base := strings.TrimRight(defaultTRPCBase, "/") + "/" + strings.Join(kiloProcedures, ",")
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	input := make(map[string]map[string]any, len(kiloProcedures))
	for i := range kiloProcedures {
		input[strconv.Itoa(i)] = map[string]any{"json": nil}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("batch", "1")
	q.Set("input", string(body))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseUsage maps Kilo's tRPC response into quota fields.
func parseUsage(root any) (usageSnapshot, error) {
	entries, err := responseEntriesByIndex(root)
	if err != nil {
		return usageSnapshot{}, err
	}
	payloads := make(map[string]any, len(kiloProcedures))
	for index, procedure := range kiloProcedures {
		entry := entries[index]
		if entry == nil {
			continue
		}
		if err := trpcError(entry); err != nil {
			if optionalProcedures[procedure] {
				continue
			}
			return usageSnapshot{}, err
		}
		if payload, ok := resultPayload(entry); ok {
			payloads[procedure] = payload
		}
	}

	used, total, remaining := creditFields(payloads[kiloProcedures[0]])
	pass := parsePassFields(payloads[kiloProcedures[1]])
	autoTopUpEnabled, autoTopUpMethod := autoTopUpState(payloads[kiloProcedures[0]], payloads[kiloProcedures[2]])
	return usageSnapshot{
		CreditsUsed:      used,
		CreditsTotal:     total,
		CreditsRemaining: remaining,
		PassUsed:         pass.Used,
		PassTotal:        pass.Total,
		PassRemaining:    pass.Remaining,
		PassBonus:        pass.Bonus,
		PassResetsAt:     pass.ResetsAt,
		PlanName:         planName(payloads[kiloProcedures[1]]),
		AutoTopUpEnabled: autoTopUpEnabled,
		AutoTopUpMethod:  autoTopUpMethod,
		UpdatedAt:        time.Now().UTC(),
	}, nil
}

// responseEntriesByIndex normalizes single and batch tRPC response shapes.
func responseEntriesByIndex(root any) (map[int]map[string]any, error) {
	switch v := root.(type) {
	case []any:
		out := make(map[int]map[string]any, len(v))
		for i, item := range v {
			if i >= len(kiloProcedures) {
				break
			}
			if m, ok := providerutil.MapValue(item); ok {
				out[i] = m
			}
		}
		return out, nil
	case map[string]any:
		if v["result"] != nil || v["error"] != nil {
			return map[int]map[string]any{0: v}, nil
		}
		out := map[int]map[string]any{}
		for key, item := range v {
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(kiloProcedures) {
				continue
			}
			if m, ok := providerutil.MapValue(item); ok {
				out[index] = m
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected Kilo tRPC response shape")
}

// trpcError maps a tRPC error entry to a provider error.
func trpcError(entry map[string]any) error {
	errorObject, ok := providerutil.MapValue(entry["error"])
	if !ok {
		return nil
	}
	combined := strings.ToLower(strings.Join([]string{
		pathString(errorObject, "json", "data", "code"),
		pathString(errorObject, "data", "code"),
		providerutil.StringValue(errorObject["code"]),
		pathString(errorObject, "json", "message"),
		providerutil.StringValue(errorObject["message"]),
	}, " "))
	switch {
	case strings.Contains(combined, "unauthorized"), strings.Contains(combined, "forbidden"):
		return errUnauthorized
	case strings.Contains(combined, "not_found"), strings.Contains(combined, "not found"):
		return fmt.Errorf("Kilo API endpoint not found")
	default:
		return fmt.Errorf("Kilo tRPC error payload")
	}
}

// resultPayload extracts the JSON payload from one tRPC result entry.
func resultPayload(entry map[string]any) (any, bool) {
	result, ok := providerutil.MapValue(entry["result"])
	if !ok {
		return nil, false
	}
	if data, ok := providerutil.MapValue(result["data"]); ok {
		if jsonPayload, found := data["json"]; found && jsonPayload != nil {
			return jsonPayload, true
		}
		return data, true
	}
	if jsonPayload, found := result["json"]; found && jsonPayload != nil {
		return jsonPayload, true
	}
	return nil, false
}

// creditFields extracts Kilo's ordinary credit balance lane.
func creditFields(payload any) (used, total, remaining *float64) {
	contexts := dictionaryContexts(payload)
	if blocks := firstArray(contexts, "creditBlocks"); len(blocks) > 0 {
		var totalSum, remainingSum float64
		var sawTotal, sawRemaining bool
		for _, block := range blocks {
			m, ok := providerutil.MapValue(block)
			if !ok {
				continue
			}
			if amount, ok := providerutil.FloatValue(m["amount_mUsd"]); ok {
				totalSum += amount / 1_000_000
				sawTotal = true
			}
			if balance, ok := providerutil.FloatValue(m["balance_mUsd"]); ok {
				remainingSum += balance / 1_000_000
				sawRemaining = true
			}
		}
		if sawTotal || sawRemaining {
			total = optionalFloat(sawTotal, math.Max(0, totalSum))
			remaining = optionalFloat(sawRemaining, math.Max(0, remainingSum))
			if total != nil && remaining != nil {
				used = floatPtr(math.Max(0, *total-*remaining))
			}
			return used, total, remaining
		}
	}

	blockContexts := contextsFromArray(firstArray(contexts, "blocks"))
	used = firstFloatPtr(blockContexts, "used", "usedCredits", "consumed", "spent", "creditsUsed")
	total = firstFloatPtr(blockContexts, "total", "totalCredits", "creditsTotal", "limit")
	remaining = firstFloatPtr(blockContexts, "remaining", "remainingCredits", "creditsRemaining")
	if used == nil {
		used = firstFloatPtr(contexts, "used", "usedCredits", "creditsUsed", "consumed", "spent")
	}
	if total == nil {
		total = firstFloatPtr(contexts, "total", "totalCredits", "creditsTotal", "limit")
	}
	if remaining == nil {
		remaining = firstFloatPtr(contexts, "remaining", "remainingCredits", "creditsRemaining")
	}
	if total == nil && used != nil && remaining != nil {
		total = floatPtr(*used + *remaining)
	}
	if used == nil && total != nil && remaining != nil {
		used = floatPtr(math.Max(0, *total-*remaining))
	}
	if used == nil && total == nil && remaining == nil {
		if balance, ok := firstFloat(contexts, "totalBalance_mUsd"); ok {
			balanceUSD := math.Max(0, balance/1_000_000)
			return floatPtr(0), floatPtr(balanceUSD), floatPtr(balanceUSD)
		}
	}
	return used, total, remaining
}

// parsePassFields extracts Kilo Pass quota fields.
func parsePassFields(payload any) passFields {
	if subscription := subscriptionData(payload); subscription != nil {
		used := moneyPtr(providerutil.FloatValue(subscription["currentPeriodUsageUsd"]))
		base := moneyPtr(providerutil.FloatValue(subscription["currentPeriodBaseCreditsUsd"]))
		bonus := 0.0
		if rawBonus, ok := providerutil.FloatValue(subscription["currentPeriodBonusCreditsUsd"]); ok {
			bonus = math.Max(0, rawBonus)
		}
		var total, remaining *float64
		if base != nil {
			total = floatPtr(*base + bonus)
		}
		if total != nil && used != nil {
			remaining = floatPtr(math.Max(0, *total-*used))
		}
		return passFields{
			Used:      used,
			Total:     total,
			Remaining: remaining,
			Bonus:     optionalFloat(bonus > 0, bonus),
			ResetsAt:  firstTimePtr([]map[string]any{subscription}, "nextBillingAt", "nextRenewalAt", "renewsAt", "renewAt"),
		}
	}

	contexts := dictionaryContexts(payload)
	used := moneyAmount(contexts,
		[]string{"usedCents", "spentCents", "consumedCents", "usedAmountCents", "consumedAmountCents"},
		[]string{"used_mUsd", "spent_mUsd", "consumed_mUsd", "usedAmount_mUsd"},
		[]string{"used", "spent", "consumed", "usage", "creditsUsed", "usedAmount", "consumedAmount"})
	total := moneyAmount(contexts,
		[]string{"amountCents", "totalCents", "planAmountCents", "monthlyAmountCents", "limitCents", "includedCents", "valueCents"},
		[]string{"amount_mUsd", "total_mUsd", "planAmount_mUsd", "limit_mUsd", "included_mUsd", "value_mUsd"},
		[]string{"amount", "total", "limit", "included", "value", "creditsTotal", "totalCredits", "planAmount"})
	remaining := moneyAmount(contexts,
		[]string{"remainingCents", "remainingAmountCents", "availableCents", "leftCents", "balanceCents"},
		[]string{"remaining_mUsd", "available_mUsd", "left_mUsd", "balance_mUsd"},
		[]string{"remaining", "available", "left", "balance", "creditsRemaining", "remainingAmount", "availableAmount"})
	bonus := moneyAmount(contexts,
		[]string{"bonusCents", "bonusAmountCents", "includedBonusCents", "bonusRemainingCents"},
		[]string{"bonus_mUsd", "bonusAmount_mUsd"},
		[]string{"bonus", "bonusAmount", "bonusCredits", "includedBonus"})
	if total == nil && used != nil && remaining != nil {
		total = floatPtr(*used + *remaining)
	}
	if used == nil && total != nil && remaining != nil {
		used = floatPtr(math.Max(0, *total-*remaining))
	}
	if remaining == nil && total != nil && used != nil {
		remaining = floatPtr(math.Max(0, *total-*used))
	}
	return passFields{
		Used:      used,
		Total:     total,
		Remaining: remaining,
		Bonus:     bonus,
		ResetsAt: firstTimePtr(contexts,
			"resetAt", "resetsAt", "nextResetAt", "renewAt", "renewsAt",
			"nextRenewalAt", "currentPeriodEnd", "periodEndsAt", "expiresAt", "expiryAt"),
	}
}

// subscriptionData returns a Kilo subscription object when present.
func subscriptionData(payload any) map[string]any {
	m, ok := providerutil.MapValue(payload)
	if !ok {
		return nil
	}
	if sub, ok := providerutil.MapValue(m["subscription"]); ok {
		return sub
	}
	if m["currentPeriodUsageUsd"] != nil ||
		m["currentPeriodBaseCreditsUsd"] != nil ||
		m["currentPeriodBonusCreditsUsd"] != nil ||
		m["tier"] != nil {
		return m
	}
	return nil
}

// planName extracts the displayable Kilo Pass plan name.
func planName(payload any) string {
	if sub := subscriptionData(payload); sub != nil {
		if tier := providerutil.StringValue(sub["tier"]); tier != "" {
			return planNameForTier(tier)
		}
		return "Kilo Pass"
	}
	contexts := dictionaryContexts(payload)
	for _, candidate := range []string{
		firstString(contexts, "planName", "tier", "tierName", "passName", "subscriptionName"),
		pathStringInContexts(contexts, "plan", "name"),
		pathStringInContexts(contexts, "subscription", "plan", "name"),
		pathStringInContexts(contexts, "subscription", "name"),
		pathStringInContexts(contexts, "pass", "name"),
		pathStringInContexts(contexts, "state", "name"),
		firstString(contexts, "state"),
	} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	if name := firstString(contexts, "name"); strings.Contains(strings.ToLower(name), "pass") {
		return name
	}
	return ""
}

// planNameForTier maps Kilo internal tier IDs to public plan names.
func planNameForTier(tier string) string {
	switch tier {
	case "tier_19":
		return "Starter"
	case "tier_49":
		return "Pro"
	case "tier_199":
		return "Expert"
	default:
		return tier
	}
}

// autoTopUpState extracts auto top-up state from Kilo payloads.
func autoTopUpState(creditBlocksPayload, autoTopUpPayload any) (*bool, string) {
	creditContexts := dictionaryContexts(creditBlocksPayload)
	autoTopUpContexts := dictionaryContexts(autoTopUpPayload)
	enabled := firstBoolPtr(autoTopUpContexts, "enabled", "isEnabled", "active")
	if enabled == nil {
		if v, ok := boolFromStatusString(firstString(autoTopUpContexts, "status")); ok {
			enabled = &v
		}
	}
	if enabled == nil {
		enabled = firstBoolPtr(creditContexts, "autoTopUpEnabled")
	}
	method := firstString(autoTopUpContexts, "paymentMethod", "paymentMethodType", "method", "cardBrand")
	if method == "" {
		if amount := moneyAmount(autoTopUpContexts, []string{"amountCents"}, nil, []string{"amount", "topUpAmount", "amountUsd"}); amount != nil && *amount > 0 {
			method = currencyLabel(*amount)
		}
	}
	return enabled, strings.TrimSpace(method)
}

// snapshotFromUsage maps parsed Kilo usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot, source string) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	var metrics []providers.MetricValue
	if usage.CreditsTotal != nil {
		used := resolvedUsed(usage.CreditsUsed, usage.CreditsTotal, usage.CreditsRemaining)
		metrics = append(metrics, percentMetric(
			"session-percent",
			"CREDITS",
			"Kilo credits remaining",
			used,
			*usage.CreditsTotal,
			nil,
			fmt.Sprintf("%s/%s credits", compactNumber(used), compactNumber(*usage.CreditsTotal)),
			now,
		))
	}
	if usage.PassTotal != nil {
		used := resolvedUsed(usage.PassUsed, usage.PassTotal, usage.PassRemaining)
		caption := fmt.Sprintf("$%s / $%s", currencyNumber(used), currencyNumber(basePassCredits(usage.PassTotal, usage.PassBonus)))
		if usage.PassBonus != nil && *usage.PassBonus > 0 {
			caption += fmt.Sprintf(" (+ $%s bonus)", currencyNumber(*usage.PassBonus))
		}
		metrics = append(metrics, percentMetric(
			"weekly-percent",
			"PASS",
			"Kilo Pass remaining",
			used,
			*usage.PassTotal,
			usage.PassResetsAt,
			caption,
			now,
		))
	}
	providerName := "Kilo"
	if usage.PlanName != "" {
		providerName += " " + usage.PlanName
	}
	if len(metrics) == 0 {
		return providers.Snapshot{
			ProviderID:   "kilo",
			ProviderName: providerName,
			Source:       source,
			Metrics:      []providers.MetricValue{},
			Status:       "unknown",
			Error:        "Kilo response missing usage data.",
		}
	}
	return providers.Snapshot{
		ProviderID:   "kilo",
		ProviderName: providerName,
		Source:       source,
		Metrics:      metrics,
		Status:       "operational",
	}
}

// percentMetric builds one Kilo remaining-percent metric.
func percentMetric(id, label, name string, used, total float64, resetAt *time.Time, caption string, now string) providers.MetricValue {
	usedPct := 100.0
	if total > 0 {
		usedPct = math.Max(0, math.Min(100, used/total*100))
	}
	m := providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
	if total > 0 {
		remaining := int(math.Round(math.Max(0, total-used)))
		maximum := int(math.Round(total))
		if maximum > 0 {
			m.RawCount = &remaining
			m.RawMax = &maximum
		}
	}
	return m
}

// dictionaryContexts returns nested JSON objects worth searching.
func dictionaryContexts(payload any) []map[string]any {
	root, ok := providerutil.MapValue(payload)
	if !ok {
		return nil
	}
	type item struct {
		object map[string]any
		depth  int
	}
	queue := []item{{object: root}}
	var out []map[string]any
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		out = append(out, cur.object)
		if cur.depth >= 2 {
			continue
		}
		for _, value := range cur.object {
			if nested, ok := providerutil.MapValue(value); ok {
				queue = append(queue, item{object: nested, depth: cur.depth + 1})
				continue
			}
			if values, ok := value.([]any); ok {
				for _, arrayValue := range values {
					if nested, ok := providerutil.MapValue(arrayValue); ok {
						queue = append(queue, item{object: nested, depth: cur.depth + 1})
					}
				}
			}
		}
	}
	return out
}

// contextsFromArray returns JSON objects from an array.
func contextsFromArray(values []any) []map[string]any {
	var out []map[string]any
	for _, value := range values {
		if m, ok := providerutil.MapValue(value); ok {
			out = append(out, m)
		}
	}
	return out
}

// firstArray returns the first array stored under any key.
func firstArray(contexts []map[string]any, keys ...string) []any {
	for _, ctx := range contexts {
		for _, key := range keys {
			if values, ok := ctx[key].([]any); ok {
				return values
			}
		}
	}
	return nil
}

// firstFloat returns the first numeric value stored under any key.
func firstFloat(contexts []map[string]any, keys ...string) (float64, bool) {
	for _, ctx := range contexts {
		if v, ok := providerutil.FirstFloat(ctx, keys...); ok {
			return v, true
		}
	}
	return 0, false
}

// firstFloatPtr returns firstFloat as a pointer.
func firstFloatPtr(contexts []map[string]any, keys ...string) *float64 {
	if v, ok := firstFloat(contexts, keys...); ok {
		return floatPtr(math.Max(0, v))
	}
	return nil
}

// firstString returns the first non-empty string-like value stored under any key.
func firstString(contexts []map[string]any, keys ...string) string {
	for _, ctx := range contexts {
		if v := providerutil.FirstString(ctx, keys...); v != "" {
			return v
		}
	}
	return ""
}

// firstBoolPtr returns the first bool-like value stored under any key.
func firstBoolPtr(contexts []map[string]any, keys ...string) *bool {
	for _, ctx := range contexts {
		for _, key := range keys {
			if v, ok := boolValue(ctx[key]); ok {
				return &v
			}
		}
	}
	return nil
}

// firstTimePtr returns the first date-like value stored under any key.
func firstTimePtr(contexts []map[string]any, keys ...string) *time.Time {
	for _, ctx := range contexts {
		if v, ok := providerutil.FirstTime(ctx, keys...); ok {
			return v
		}
	}
	return nil
}

// moneyAmount returns a currency amount from cents, micro-dollar, or plain dollar keys.
func moneyAmount(contexts []map[string]any, centsKeys, microUSDKeys, plainKeys []string) *float64 {
	if v, ok := firstFloat(contexts, centsKeys...); ok {
		return floatPtr(math.Max(0, v/100))
	}
	if v, ok := firstFloat(contexts, microUSDKeys...); ok {
		return floatPtr(math.Max(0, v/1_000_000))
	}
	if v, ok := firstFloat(contexts, plainKeys...); ok {
		return floatPtr(math.Max(0, v))
	}
	return nil
}

// pathString returns a nested string-like value.
func pathString(root map[string]any, keys ...string) string {
	var cur any = root
	for _, key := range keys {
		m, ok := providerutil.MapValue(cur)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	return providerutil.StringValue(cur)
}

// pathStringInContexts returns a nested string-like value from the first matching context.
func pathStringInContexts(contexts []map[string]any, keys ...string) string {
	for _, ctx := range contexts {
		if v := pathString(ctx, keys...); v != "" {
			return v
		}
	}
	return ""
}

// boolValue converts a JSON scalar into a bool.
func boolValue(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		return boolFromStatusString(v)
	case float64:
		return v != 0, true
	default:
		return false, false
	}
}

// boolFromStatusString maps common status strings to bool.
func boolFromStatusString(status string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "true", "1", "yes", "enabled", "active", "on":
		return true, true
	case "false", "0", "no", "disabled", "inactive", "off", "none":
		return false, true
	default:
		return false, false
	}
}

// resolvedUsed derives a non-negative used amount.
func resolvedUsed(used, total, remaining *float64) float64 {
	if used != nil {
		return math.Max(0, *used)
	}
	if total != nil && remaining != nil {
		return math.Max(0, *total-*remaining)
	}
	return 0
}

// basePassCredits subtracts bonus credits from the Kilo Pass total.
func basePassCredits(total, bonus *float64) float64 {
	if total == nil {
		return 0
	}
	if bonus == nil {
		return *total
	}
	return math.Max(0, *total-*bonus)
}

// moneyPtr turns a parsed number into a non-negative pointer.
func moneyPtr(value float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return floatPtr(math.Max(0, value))
}

// optionalFloat returns a pointer only when ok is true.
func optionalFloat(ok bool, value float64) *float64 {
	if !ok {
		return nil
	}
	return floatPtr(value)
}

// floatPtr returns a pointer to v.
func floatPtr(v float64) *float64 {
	return &v
}

// compactNumber formats a Kilo credit count.
func compactNumber(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f", value)
	}
	return fmt.Sprintf("%.2f", value)
}

// currencyNumber formats a Kilo Pass dollar amount.
func currencyNumber(value float64) string {
	return fmt.Sprintf("%.2f", math.Max(0, value))
}

// currencyLabel formats an auto top-up amount label.
func currencyLabel(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("$%.0f", value)
	}
	return fmt.Sprintf("$%.2f", value)
}

// init registers the Kilo provider with the package registry.
func init() {
	providers.Register(Provider{})
}
