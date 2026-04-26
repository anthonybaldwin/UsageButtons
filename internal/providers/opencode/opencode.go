// Package opencode implements the OpenCode usage provider.
//
// Auth: Usage Buttons Helper extension with the user's opencode.ai browser
// session. Endpoints: POST/GET https://opencode.ai/_server.
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
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
	baseURL              = "https://opencode.ai"
	serverURL            = "https://opencode.ai/_server"
	workspacesServerID   = "def39973159c7f0483d8793a822b8dbb10d067e12c65455fcb4608459ba0234f"
	subscriptionServerID = "7abeebee372f304e050aaaf92be863f4a86490e382f8c79db68fd94040d691b4"
)

var workspaceIDRE = regexp.MustCompile(`id\s*:\s*\\?"(wrk_[^\\"]+)`)

// usageSnapshot is OpenCode rolling and weekly usage.
type usageSnapshot struct {
	RollingUsagePercent float64
	WeeklyUsagePercent  float64
	RollingResetInSec   int
	WeeklyResetInSec    int
	UpdatedAt           time.Time
}

// windowCandidate is one parsed usage window from a flexible JSON payload.
type windowCandidate struct {
	Percent    float64
	ResetInSec int
	PathLower  string
}

// Provider fetches OpenCode usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "opencode" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "OpenCode" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#3b82f6" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#081a33" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest OpenCode usage snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return errorSnapshot(cookieaux.MissingMessage("opencode.ai")), nil
	}
	usage, err := fetchUsage(ctx)
	if err != nil {
		var httpErr *httputil.Error
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		if looksSignedOut(err.Error()) {
			return errorSnapshot(cookieaux.StaleMessage("opencode.ai")), nil
		}
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsage fetches workspace and subscription usage data.
func fetchUsage(ctx context.Context) (usageSnapshot, error) {
	workspaceID, err := workspaceID(ctx)
	if err != nil {
		return usageSnapshot{}, err
	}
	text, err := serverText(ctx, serverRequest{
		ServerID: subscriptionServerID,
		Args:     []any{workspaceID},
		Method:   "GET",
		Referer:  fmt.Sprintf("%s/workspace/%s/billing", baseURL, workspaceID),
	})
	if err != nil {
		return usageSnapshot{}, err
	}
	if looksSignedOut(text) {
		return usageSnapshot{}, fmt.Errorf("OpenCode session is signed out")
	}
	// POST fallback only when the GET response is null or unrecognized.
	// Recognized empty-state payloads (no subscription / no recorded
	// windows) are valid responses, so don't waste a second round-trip.
	if (isNullPayload(text) || !subscriptionLooksUsable(text)) && !looksLikeEmptyUsage(text) {
		fallback, fallbackErr := serverText(ctx, serverRequest{
			ServerID: subscriptionServerID,
			Args:     []any{workspaceID},
			Method:   "POST",
			Referer:  fmt.Sprintf("%s/workspace/%s/billing", baseURL, workspaceID),
		})
		if fallbackErr == nil && !isNullPayload(fallback) {
			text = fallback
		}
	}
	return parseSubscription(text, time.Now().UTC())
}

// workspaceID returns an override or discovers the first OpenCode workspace.
func workspaceID(ctx context.Context) (string, error) {
	return WorkspaceID(ctx, "CODEXBAR_OPENCODE_WORKSPACE_ID")
}

// WorkspaceID returns an override or discovers the first OpenCode workspace.
func WorkspaceID(ctx context.Context, envName string) (string, error) {
	if override := normalizeWorkspaceID(os.Getenv(envName)); override != "" {
		return override, nil
	}
	text, err := serverText(ctx, serverRequest{
		ServerID: workspacesServerID,
		Method:   "GET",
		Referer:  baseURL,
	})
	if err != nil {
		return "", err
	}
	if looksSignedOut(text) {
		return "", fmt.Errorf("OpenCode session is signed out")
	}
	ids := parseWorkspaceIDs(text)
	if len(ids) == 0 {
		fallback, fallbackErr := serverText(ctx, serverRequest{
			ServerID: workspacesServerID,
			Args:     []any{},
			Method:   "POST",
			Referer:  baseURL,
		})
		if fallbackErr == nil {
			ids = parseWorkspaceIDs(fallback)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("OpenCode response missing workspace id")
	}
	return ids[0], nil
}

// serverRequest describes one OpenCode _server call.
type serverRequest struct {
	ServerID string
	Args     []any
	Method   string
	Referer  string
}

// serverText calls an OpenCode _server endpoint through the Helper.
func serverText(ctx context.Context, req serverRequest) (string, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	rawURL, body, err := serverRequestURL(req.ServerID, req.Args, method)
	if err != nil {
		return "", err
	}
	headers := map[string]string{
		"Accept":            "text/javascript, application/json;q=0.9, */*;q=0.8",
		"Origin":            baseURL,
		"Referer":           req.Referer,
		"X-Server-Id":       req.ServerID,
		"X-Server-Instance": "server-fn:" + newRequestID(),
	}
	if method != "GET" {
		headers["Content-Type"] = "application/json"
	}
	resp, err := cookies.Fetch(ctx, cookies.Request{
		URL:     rawURL,
		Method:  method,
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", &httputil.Error{
			Status:     resp.Status,
			StatusText: resp.StatusText,
			Body:       string(resp.Body),
			URL:        rawURL,
		}
	}
	return string(resp.Body), nil
}

// serverRequestURL builds the _server URL and optional JSON body.
func serverRequestURL(serverID string, args []any, method string) (string, []byte, error) {
	if method != "GET" {
		body, err := json.Marshal(args)
		return serverURL, body, err
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", nil, err
	}
	q := u.Query()
	q.Set("id", serverID)
	if len(args) > 0 {
		body, err := json.Marshal(args)
		if err != nil {
			return "", nil, err
		}
		q.Set("args", string(body))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil, nil
}

// parseSubscription parses rolling and weekly usage from text or JSON.
func parseSubscription(text string, now time.Time) (usageSnapshot, error) {
	if usage, ok := parseSubscriptionJSON(text, now); ok {
		return usage, nil
	}
	rollingPercent := extractFloat(`rollingUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	rollingReset := extractInt(`rollingUsage[^}]*?resetInSec\s*:\s*([0-9]+)`, text)
	weeklyPercent := extractFloat(`weeklyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`, text)
	weeklyReset := extractInt(`weeklyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`, text)
	if rollingPercent != nil && rollingReset != nil && weeklyPercent != nil && weeklyReset != nil {
		return usageSnapshot{
			RollingUsagePercent: clampPercent(*rollingPercent),
			WeeklyUsagePercent:  clampPercent(*weeklyPercent),
			RollingResetInSec:   *rollingReset,
			WeeklyResetInSec:    *weeklyReset,
			UpdatedAt:           now,
		}, nil
	}
	// Workspaces with no active subscription or no recorded windows return
	// Solid SSR hydration payloads that lack rollingUsage/weeklyUsage entirely
	// (e.g. {usage:[],keys:[...]} or {subscription:null,...}). Surface these
	// as zero usage rather than a parse error so the button stays useful.
	if looksLikeEmptyUsage(text) {
		return usageSnapshot{UpdatedAt: now}, nil
	}
	dumpUnknownResponse(text)
	return usageSnapshot{}, fmt.Errorf("OpenCode parse error: missing usage fields")
}

// looksLikeEmptyUsage reports whether text is an OpenCode _server response
// that conveys "no rolling/weekly usage" rather than a schema break.
// usagePercent is the field every populated response carries — its absence
// (combined with a Solid SSR marker) is a reliable empty-state signal. The
// schema key names (rollingUsage/weeklyUsage) can themselves appear with
// `null` values in empty responses, so we don't short-circuit on those.
func looksLikeEmptyUsage(text string) bool {
	if strings.Contains(text, "usagePercent") {
		return false
	}
	if !strings.Contains(text, "server-fn:") {
		return false
	}
	compact := strings.Join(strings.Fields(text), "")
	// Solid SSR may resolve the entire server-fn payload to null —
	// the response then ends with `,null)` after the array assignment.
	if strings.HasSuffix(compact, ",null)") {
		return true
	}
	for _, marker := range []string{
		"rollingUsage:null",
		"weeklyUsage:null",
		"subscription:null",
		"subscriptionPlan:null",
		"monthlyUsage:null",
		"monthlyLimit:null",
		"usage:$R",
		"usage:[]",
		"keys:$R",
		"keys:[]",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

// parseSubscriptionJSON parses flexible JSON usage payloads.
func parseSubscriptionJSON(text string, now time.Time) (usageSnapshot, bool) {
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return usageSnapshot{}, false
	}
	var candidates []windowCandidate
	collectWindowCandidates(raw, now, nil, &candidates)
	if len(candidates) == 0 {
		return usageSnapshot{}, false
	}
	rolling := pickWindow(candidates, true, "rolling", "hour", "5h", "5-hour")
	weekly := pickWindow(candidates, false, "weekly", "week")
	if rolling == nil {
		rolling = pickAnyWindow(candidates, true, nil)
	}
	if weekly == nil {
		weekly = pickAnyWindow(candidates, false, rolling)
	}
	if rolling == nil || weekly == nil {
		return usageSnapshot{}, false
	}
	return usageSnapshot{
		RollingUsagePercent: rolling.Percent,
		WeeklyUsagePercent:  weekly.Percent,
		RollingResetInSec:   rolling.ResetInSec,
		WeeklyResetInSec:    weekly.ResetInSec,
		UpdatedAt:           now,
	}, true
}

// collectWindowCandidates finds quota-like objects in arbitrary JSON.
func collectWindowCandidates(value any, now time.Time, path []string, out *[]windowCandidate) {
	switch v := value.(type) {
	case map[string]any:
		if window, ok := parseWindow(v, now); ok {
			*out = append(*out, windowCandidate{
				Percent:    window.Percent,
				ResetInSec: window.ResetInSec,
				PathLower:  strings.ToLower(strings.Join(path, ".")),
			})
		}
		for key, item := range v {
			collectWindowCandidates(item, now, append(path, key), out)
		}
	case []any:
		for i, item := range v {
			collectWindowCandidates(item, now, append(path, fmt.Sprintf("[%d]", i)), out)
		}
	}
}

// parsedWindow is one quota window parsed from a JSON object.
type parsedWindow struct {
	Percent    float64
	ResetInSec int
}

// parseWindow extracts percent and reset data from a JSON object.
func parseWindow(m map[string]any, now time.Time) (parsedWindow, bool) {
	percentKeys := []string{
		"usagePercent", "usedPercent", "percentUsed", "percent",
		"usage_percent", "used_percent", "utilization",
		"utilizationPercent", "utilization_percent", "usage",
	}
	resetInKeys := []string{
		"resetInSec", "resetInSeconds", "resetSeconds", "reset_sec",
		"reset_in_sec", "resetsInSec", "resetsInSeconds", "resetIn", "resetSec",
	}
	resetAtKeys := []string{
		"resetAt", "resetsAt", "reset_at", "resets_at",
		"nextReset", "next_reset", "renewAt", "renew_at",
	}
	percent, ok := providerutil.FirstFloat(m, percentKeys...)
	if !ok {
		used, usedOK := providerutil.FirstFloat(m, "used", "usage", "consumed", "count", "usedTokens")
		limit, limitOK := providerutil.FirstFloat(m, "limit", "total", "quota", "max", "cap", "tokenLimit")
		if usedOK && limitOK && limit > 0 {
			percent = used / limit * 100
			ok = true
		}
	}
	if !ok {
		return parsedWindow{}, false
	}
	reset, resetOK := providerutil.FirstFloat(m, resetInKeys...)
	if !resetOK {
		if resetAt, ok := providerutil.FirstTime(m, resetAtKeys...); ok {
			reset = math.Max(0, resetAt.Sub(now).Seconds())
			resetOK = true
		}
	}
	if !resetOK {
		reset = 0
	}
	return parsedWindow{
		Percent:    clampPercent(percent),
		ResetInSec: int(math.Round(reset)),
	}, true
}

// pickWindow chooses a candidate matching one of the path hints.
func pickWindow(candidates []windowCandidate, pickShorter bool, hints ...string) *windowCandidate {
	var filtered []windowCandidate
	for _, candidate := range candidates {
		for _, hint := range hints {
			if strings.Contains(candidate.PathLower, hint) {
				filtered = append(filtered, candidate)
				break
			}
		}
	}
	return pickAnyWindow(filtered, pickShorter, nil)
}

// pickAnyWindow chooses by shortest or longest reset.
func pickAnyWindow(candidates []windowCandidate, pickShorter bool, excluding *windowCandidate) *windowCandidate {
	var picked *windowCandidate
	for _, candidate := range candidates {
		if excluding != nil && candidate.PathLower == excluding.PathLower && candidate.ResetInSec == excluding.ResetInSec {
			continue
		}
		c := candidate
		if picked == nil {
			picked = &c
			continue
		}
		if pickShorter {
			if candidate.ResetInSec < picked.ResetInSec {
				picked = &c
			}
		} else if candidate.ResetInSec > picked.ResetInSec {
			picked = &c
		}
	}
	return picked
}

// snapshotFromUsage maps parsed OpenCode usage into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	metrics := []providers.MetricValue{
		percentMetric("session-percent", "5-HOUR", "OpenCode five-hour usage remaining", usage.RollingUsagePercent, usage.RollingResetInSec, "5h window", now),
		percentMetric("weekly-percent", "WEEKLY", "OpenCode weekly usage remaining", usage.WeeklyUsagePercent, usage.WeeklyResetInSec, "7d window", now),
	}
	return providers.Snapshot{
		ProviderID:   "opencode",
		ProviderName: "OpenCode",
		Source:       "cookie",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// percentMetric builds a remaining-percent OpenCode metric.
func percentMetric(id, label, name string, usedPct float64, resetSeconds int, caption string, now string) providers.MetricValue {
	var resetAt *time.Time
	if resetSeconds > 0 {
		t := time.Now().Add(time.Duration(resetSeconds) * time.Second)
		resetAt = &t
	}
	return providerutil.PercentRemainingMetric(id, label, name, usedPct, resetAt, caption, now)
}

// parseWorkspaceIDs finds workspace IDs in serialized text or JSON.
func parseWorkspaceIDs(text string) []string {
	found := uniqueStrings(workspaceIDRE.FindAllStringSubmatch(text, -1))
	if len(found) > 0 {
		return found
	}
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return nil
	}
	var out []string
	collectWorkspaceIDs(raw, &out)
	return out
}

// collectWorkspaceIDs walks arbitrary JSON looking for wrk_ strings.
func collectWorkspaceIDs(value any, out *[]string) {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "wrk_") && !containsString(*out, v) {
			*out = append(*out, v)
		}
	case []any:
		for _, item := range v {
			collectWorkspaceIDs(item, out)
		}
	case map[string]any:
		for _, item := range v {
			collectWorkspaceIDs(item, out)
		}
	}
}

// uniqueStrings returns regex capture group 1 values without duplicates.
func uniqueStrings(matches [][]string) []string {
	var out []string
	for _, match := range matches {
		if len(match) > 1 && !containsString(out, match[1]) {
			out = append(out, match[1])
		}
	}
	return out
}

// containsString reports whether values contains needle.
func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// normalizeWorkspaceID extracts a wrk_ identifier from text or URL.
func normalizeWorkspaceID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "wrk_") {
		return trimmed
	}
	if u, err := url.Parse(trimmed); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, part := range parts {
			if part == "workspace" && i+1 < len(parts) && strings.HasPrefix(parts[i+1], "wrk_") {
				return parts[i+1]
			}
		}
	}
	re := regexp.MustCompile(`wrk_[A-Za-z0-9]+`)
	return re.FindString(trimmed)
}

// subscriptionLooksUsable reports whether text likely contains usage data.
func subscriptionLooksUsable(text string) bool {
	return strings.Contains(text, "rollingUsage") ||
		strings.Contains(text, "weeklyUsage") ||
		strings.Contains(text, "usagePercent")
}

// isNullPayload reports explicit null responses.
func isNullPayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.EqualFold(trimmed, "null")
}

// looksSignedOut reports whether text is an auth/login response.
func looksSignedOut(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "login") ||
		strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "auth/authorize") ||
		strings.Contains(lower, "not associated with an account") ||
		strings.Contains(lower, `actor of type "public"`)
}

// extractFloat extracts a float from the first capture group.
func extractFloat(pattern string, text string) *float64 {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

// extractInt extracts an int from the first capture group.
func extractInt(pattern string, text string) *int {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	v, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &v
}

// clampPercent normalizes 0..1 or 0..100 values to 0..100.
func clampPercent(value float64) float64 {
	if value >= 0 && value <= 1 {
		value *= 100
	}
	return math.Max(0, math.Min(100, value))
}

// errorSnapshot returns an OpenCode setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "opencode",
		ProviderName: "OpenCode",
		Source:       "cookie",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// newRequestID returns a v4-style UUID string used in X-Server-Instance.
// OpenCode's server appears to expect a unique ID per call (CodexBar parity).
// crypto/rand.Read failures are best-effort: an all-zero buffer still formats
// as a valid v4-shape, so the header value stays UUID-shaped either way.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dumpUnknownResponse appends a truncated OpenCode response to a temp file
// when parseSubscription can't classify it. Helps diagnose new empty-state
// shapes without asking the user to enable verbose logging. Owner-only perms
// (0o600) so the response — which may contain workspace IDs / billing data —
// is not world-readable. Append mode preserves earlier shapes; a per-call
// snippet cap and total-file cap keep growth bounded.
func dumpUnknownResponse(text string) {
	const (
		maxSnippetBytes = 16 * 1024
		maxFileBytes    = 256 * 1024
	)
	path := filepath.Join(os.TempDir(), "usagebuttons-opencode-debug.txt")
	if info, err := os.Stat(path); err == nil && info.Size() >= maxFileBytes {
		return
	}
	snippet := text
	truncated := false
	if len(snippet) > maxSnippetBytes {
		snippet = snippet[:maxSnippetBytes]
		truncated = true
	}
	body := fmt.Sprintf("[%s] length=%d truncated=%v\n%s\n\n",
		time.Now().UTC().Format(time.RFC3339), len(text), truncated, snippet)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(body)
}

// init registers the OpenCode provider with the package registry.
func init() {
	providers.Register(Provider{})
}
