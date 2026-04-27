// Package vertexai implements the Google Vertex AI quota provider.
//
// Auth: gcloud Application Default Credentials from
// application_default_credentials.json. Endpoint: Cloud Monitoring
// timeSeries for aiplatform.googleapis.com quota usage and limits.
package vertexai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	providerID         = "vertexai"
	providerName       = "Vertex AI"
	defaultTimeout     = 35 * time.Second
	adcFile            = "application_default_credentials.json"
	defaultConfigFile  = "config_default"
	monitoringEndpoint = "https://monitoring.googleapis.com/v3/projects"
	tokenEndpoint      = "https://oauth2.googleapis.com/token"
	usageWindow        = 24 * time.Hour
)

var (
	errCredentialsNotFound = errors.New("gcloud credentials not found. Run gcloud auth application-default login")
	errMissingTokens       = errors.New("gcloud credentials exist but contain no refresh token")
	errMissingClient       = errors.New("gcloud credentials missing client ID or secret")
	errServiceAccount      = errors.New("service account credentials are not supported. Use gcloud auth application-default login")
	errNoProject           = errors.New("no Google Cloud project configured. Run gcloud config set project PROJECT_ID")
	errNoQuotaData         = errors.New("no Vertex AI quota data found for the current project")
)

// Provider fetches Vertex AI quota usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return providerID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return providerName }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#4285f4" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#071426" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent"}
}

// Fetch returns the latest Vertex AI quota snapshot.
//
// Demand-fetching note (FetchContext.ActiveMetricIDs intentionally
// ignored): both metrics this provider emits — session-percent
// (request quota) and weekly-percent (token quota) — are derived
// from the same usage-vs-limit Cloud Monitoring time-series pair.
// There's no per-metric endpoint to skip, so consulting the active
// set wouldn't reduce traffic. Listed in
// plans/fetchcontext-active-metrics.md as a Phase 1 candidate but
// closed out here as "no work to do" so a future reader doesn't
// re-litigate the decision. The ActiveMetricIDs plumbing in
// FetchContext is still present and would be honoured if Vertex
// ever grew per-metric endpoints (e.g. a TPU-quota series separate
// from request quota).
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	snapshot, err := fetchSnapshot(ctx)
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshot, nil
}

// oauthCredentials mirrors gcloud ADC user credentials.
type oauthCredentials struct {
	AccessToken  string
	RefreshToken string
	ClientID     string
	ClientSecret string
	ProjectID    string
	Email        string
	ExpiryDate   *time.Time
}

// quotaUsage is the parsed Cloud Monitoring quota state.
type quotaUsage struct {
	RequestsUsedPercent float64
	TokensUsedPercent   *float64
	ProjectID           string
	Email               string
}

// monitoringTimeSeriesResponse mirrors Cloud Monitoring timeSeries.list.
type monitoringTimeSeriesResponse struct {
	TimeSeries    []monitoringTimeSeries `json:"timeSeries"`
	NextPageToken string                 `json:"nextPageToken"`
}

// monitoringTimeSeries is one Cloud Monitoring series.
type monitoringTimeSeries struct {
	Metric   monitoringMetric   `json:"metric"`
	Resource monitoringResource `json:"resource"`
	Points   []monitoringPoint  `json:"points"`
}

// monitoringMetric carries metric type and labels.
type monitoringMetric struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
}

// monitoringResource carries monitored resource type and labels.
type monitoringResource struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
}

// monitoringPoint is one aligned Monitoring point.
type monitoringPoint struct {
	Value monitoringValue `json:"value"`
}

// monitoringValue is a Cloud Monitoring numeric value.
type monitoringValue struct {
	DoubleValue *float64 `json:"doubleValue"`
	Int64Value  string   `json:"int64Value"`
}

// quotaKey matches a quota usage series to its corresponding limit series.
type quotaKey struct {
	QuotaMetric string
	LimitName   string
	Location    string
}

// quotaPercent is a matched usage/limit percent for one quota key.
type quotaPercent struct {
	Key        quotaKey
	UsedPct    float64
	UsageValue float64
	LimitValue float64
}

// fetchSnapshot loads ADC credentials, refreshes an access token if needed,
// queries Cloud Monitoring quota series, and maps them to button metrics.
func fetchSnapshot(ctx context.Context) (providers.Snapshot, error) {
	creds, err := loadCredentials()
	if err != nil {
		return providers.Snapshot{}, err
	}
	if creds.ProjectID == "" {
		return providers.Snapshot{}, errNoProject
	}
	if needsRefresh(creds) {
		refreshed, err := refreshAccessToken(ctx, creds)
		if err != nil {
			return providers.Snapshot{}, err
		}
		creds.AccessToken = refreshed.AccessToken
		creds.Email = firstNonEmpty(refreshed.Email, creds.Email)
		creds.ExpiryDate = refreshed.ExpiryDate
	}
	if creds.AccessToken == "" {
		return providers.Snapshot{}, errMissingTokens
	}

	usage, err := fetchQuotaUsage(ctx, creds)
	if err != nil {
		return providers.Snapshot{}, err
	}
	return snapshotFromUsage(usage), nil
}

// loadCredentials reads gcloud ADC credentials and active project settings.
func loadCredentials() (oauthCredentials, error) {
	path := credentialsPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return oauthCredentials{}, errCredentialsNotFound
	}
	if err != nil {
		return oauthCredentials{}, fmt.Errorf("read gcloud credentials: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return oauthCredentials{}, fmt.Errorf("decode gcloud credentials: %w", err)
	}
	if providerutil.StringValue(raw["client_email"]) != "" && providerutil.StringValue(raw["private_key"]) != "" {
		return oauthCredentials{}, errServiceAccount
	}
	clientID := providerutil.StringValue(raw["client_id"])
	clientSecret := providerutil.StringValue(raw["client_secret"])
	if clientID == "" || clientSecret == "" {
		return oauthCredentials{}, errMissingClient
	}
	refreshToken := providerutil.StringValue(raw["refresh_token"])
	if refreshToken == "" {
		return oauthCredentials{}, errMissingTokens
	}
	projectID := firstNonEmpty(
		loadProjectID(),
		providerutil.StringValue(raw["quota_project_id"]),
		envProjectID(),
	)
	return oauthCredentials{
		AccessToken:  providerutil.StringValue(raw["access_token"]),
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		ProjectID:    projectID,
		Email:        emailFromIDToken(providerutil.StringValue(raw["id_token"])),
		ExpiryDate:   parseTokenExpiry(providerutil.StringValue(raw["token_expiry"])),
	}, nil
}

// credentialsPath returns the gcloud ADC credentials path for the platform.
func credentialsPath() string {
	return filepath.Join(gcloudConfigDir(), adcFile)
}

// gcloudConfigDir returns CLOUDSDK_CONFIG or the platform gcloud config dir.
func gcloudConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("CLOUDSDK_CONFIG")); v != "" {
		return filepath.Clean(v)
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		path := filepath.Join(appData, "gcloud")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "gcloud")
}

// loadProjectID reads gcloud's active project from config_default.
func loadProjectID() string {
	data, err := os.ReadFile(filepath.Join(gcloudConfigDir(), "configurations", defaultConfigFile))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "project") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

// envProjectID returns the first standard Google Cloud project env var.
func envProjectID() string {
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// needsRefresh reports whether the stored access token is missing or stale.
func needsRefresh(creds oauthCredentials) bool {
	if creds.AccessToken == "" || creds.ExpiryDate == nil {
		return true
	}
	return time.Now().Add(5 * time.Minute).After(*creds.ExpiryDate)
}

// refreshAccessToken exchanges a gcloud refresh token for a bearer token.
func refreshAccessToken(ctx context.Context, creds oauthCredentials) (oauthCredentials, error) {
	form := url.Values{}
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	form.Set("refresh_token", creds.RefreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthCredentials{}, fmt.Errorf("build Vertex AI token refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return oauthCredentials{}, fmt.Errorf("Vertex AI token refresh failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return oauthCredentials{}, fmt.Errorf("read Vertex AI token refresh response: %w", err)
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		if code := errorCode(body); code == "invalid_grant" || code == "unauthorized_client" {
			return oauthCredentials{}, fmt.Errorf("refresh token expired or revoked. Run gcloud auth application-default login again")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oauthCredentials{}, &httputil.Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        tokenEndpoint,
			Headers:    resp.Header,
		}
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return oauthCredentials{}, fmt.Errorf("parse Vertex AI token refresh: %w", err)
	}
	accessToken := providerutil.StringValue(root["access_token"])
	if accessToken == "" {
		return oauthCredentials{}, fmt.Errorf("Vertex AI token refresh response missing access token")
	}
	expiresIn, _ := providerutil.FloatValue(root["expires_in"])
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)

	_ = saveCredentials(credentialsPath(), root)

	return oauthCredentials{
		AccessToken: accessToken,
		Email:       emailFromIDToken(providerutil.StringValue(root["id_token"])),
		ExpiryDate:  &expiry,
	}, nil
}

// saveCredentials updates application_default_credentials.json with refreshed
// OAuth tokens while preserving unrelated fields.
func saveCredentials(path string, refresh map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	for _, key := range []string{"access_token", "id_token"} {
		if v := providerutil.StringValue(refresh[key]); v != "" {
			root[key] = v
		}
	}
	if expiresIn, ok := providerutil.FloatValue(refresh["expires_in"]); ok && expiresIn > 0 {
		root["token_expiry"] = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	return providerutil.WriteJSONAtomic(path, root)
}

// errorCode extracts OAuth error codes from a failed token response.
func errorCode(body []byte) string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	return providerutil.StringValue(root["error"])
}

// fetchQuotaUsage calls Cloud Monitoring and parses matched usage/limit pairs.
func fetchQuotaUsage(ctx context.Context, creds oauthCredentials) (quotaUsage, error) {
	usageSeries, err := fetchTimeSeries(ctx, creds.AccessToken, creds.ProjectID, usageFilter())
	if err != nil {
		return quotaUsage{}, err
	}
	limitSeries, err := fetchTimeSeries(ctx, creds.AccessToken, creds.ProjectID, limitFilter())
	if err != nil {
		return quotaUsage{}, err
	}
	percents, err := matchedQuotaPercents(usageSeries, limitSeries)
	if err != nil {
		return quotaUsage{}, err
	}
	requests := maxQuotaPercent(percents, isRequestQuota)
	if requests == nil {
		requests = maxQuotaPercent(percents, func(quotaKey) bool { return true })
	}
	if requests == nil {
		return quotaUsage{}, errNoQuotaData
	}
	var tokenPct *float64
	if tokens := maxQuotaPercent(percents, isTokenQuota); tokens != nil {
		tokenPct = &tokens.UsedPct
	}
	return quotaUsage{
		RequestsUsedPercent: requests.UsedPct,
		TokensUsedPercent:   tokenPct,
		ProjectID:           creds.ProjectID,
		Email:               creds.Email,
	}, nil
}

// usageFilter returns the Monitoring filter for quota allocation usage.
func usageFilter() string {
	return `metric.type="serviceruntime.googleapis.com/quota/allocation/usage" AND resource.type="consumer_quota" AND resource.label.service="aiplatform.googleapis.com"`
}

// limitFilter returns the Monitoring filter for quota allocation limits.
func limitFilter() string {
	return `metric.type="serviceruntime.googleapis.com/quota/limit" AND resource.type="consumer_quota" AND resource.label.service="aiplatform.googleapis.com"`
}

// fetchTimeSeries pages through Cloud Monitoring timeSeries.list.
func fetchTimeSeries(ctx context.Context, accessToken, projectID, filter string) ([]monitoringTimeSeries, error) {
	now := time.Now().UTC()
	start := now.Add(-usageWindow)
	pageToken := ""
	var out []monitoringTimeSeries
	for {
		endpoint, err := monitoringURL(projectID, filter, start, now, pageToken)
		if err != nil {
			return nil, err
		}
		var resp monitoringTimeSeriesResponse
		if err := getJSON(ctx, endpoint, accessToken, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.TimeSeries...)
		pageToken = strings.TrimSpace(resp.NextPageToken)
		if pageToken == "" {
			return out, nil
		}
	}
}

// monitoringURL builds a Cloud Monitoring timeSeries.list URL.
func monitoringURL(projectID, filter string, start, end time.Time, pageToken string) (string, error) {
	u, err := url.Parse(monitoringEndpoint + "/" + url.PathEscape(projectID) + "/timeSeries")
	if err != nil {
		return "", fmt.Errorf("build Monitoring URL: %w", err)
	}
	q := u.Query()
	q.Set("filter", filter)
	q.Set("interval.startTime", start.Format(time.RFC3339))
	q.Set("interval.endTime", end.Format(time.RFC3339))
	q.Set("aggregation.alignmentPeriod", "3600s")
	q.Set("aggregation.perSeriesAligner", "ALIGN_MAX")
	q.Set("view", "FULL")
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// getJSON performs an authenticated GET and decodes a JSON response.
func getJSON(ctx context.Context, endpoint, accessToken string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build Vertex AI request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Vertex AI request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("Vertex AI request unauthorized. Run gcloud auth application-default login")
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("access forbidden. Check IAM permissions for Cloud Monitoring")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httputil.Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        endpoint,
			Headers:    resp.Header,
		}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parse Vertex AI response: %w", err)
	}
	return nil
}

// matchedQuotaPercents joins quota usage series to limit series by key.
func matchedQuotaPercents(usageSeries, limitSeries []monitoringTimeSeries) ([]quotaPercent, error) {
	usageByKey := aggregateSeries(usageSeries)
	limitByKey := aggregateSeries(limitSeries)
	if len(usageByKey) == 0 || len(limitByKey) == 0 {
		return nil, errNoQuotaData
	}
	var out []quotaPercent
	for key, limit := range limitByKey {
		if limit <= 0 {
			continue
		}
		usage, ok := usageByKey[key]
		if !ok {
			continue
		}
		usedPct := math.Max(0, math.Min(100, usage/limit*100))
		out = append(out, quotaPercent{Key: key, UsedPct: usedPct, UsageValue: usage, LimitValue: limit})
	}
	if len(out) == 0 {
		return nil, errNoQuotaData
	}
	return out, nil
}

// aggregateSeries returns the maximum point value for each quota key.
func aggregateSeries(series []monitoringTimeSeries) map[quotaKey]float64 {
	out := map[quotaKey]float64{}
	for _, item := range series {
		key, ok := seriesQuotaKey(item)
		if !ok {
			continue
		}
		value, ok := maxPointValue(item.Points)
		if !ok {
			continue
		}
		if value > out[key] {
			out[key] = value
		}
	}
	return out
}

// seriesQuotaKey extracts the matching key from Monitoring labels.
func seriesQuotaKey(series monitoringTimeSeries) (quotaKey, bool) {
	quotaMetric := firstNonEmpty(series.Metric.Labels["quota_metric"], series.Resource.Labels["quota_id"])
	if quotaMetric == "" {
		return quotaKey{}, false
	}
	location := series.Resource.Labels["location"]
	if location == "" {
		location = "global"
	}
	return quotaKey{
		QuotaMetric: quotaMetric,
		LimitName:   series.Metric.Labels["limit_name"],
		Location:    location,
	}, true
}

// maxPointValue returns the highest numeric point in a series.
func maxPointValue(points []monitoringPoint) (float64, bool) {
	var picked float64
	found := false
	for _, point := range points {
		value, ok := pointValue(point)
		if !ok {
			continue
		}
		if !found || value > picked {
			picked = value
			found = true
		}
	}
	return picked, found
}

// pointValue extracts a double or int64 Cloud Monitoring point value.
func pointValue(point monitoringPoint) (float64, bool) {
	if point.Value.DoubleValue != nil {
		return *point.Value.DoubleValue, true
	}
	if point.Value.Int64Value != "" {
		v, err := strconv.ParseFloat(point.Value.Int64Value, 64)
		return v, err == nil
	}
	return 0, false
}

// maxQuotaPercent returns the highest usage percent matching a predicate.
func maxQuotaPercent(percents []quotaPercent, match func(quotaKey) bool) *quotaPercent {
	var picked quotaPercent
	found := false
	for _, pct := range percents {
		if !match(pct.Key) {
			continue
		}
		if !found || pct.UsedPct > picked.UsedPct {
			picked = pct
			found = true
		}
	}
	if !found {
		return nil
	}
	return &picked
}

// isRequestQuota reports whether a quota key appears request-based.
func isRequestQuota(key quotaKey) bool {
	text := strings.ToLower(key.QuotaMetric + " " + key.LimitName)
	return strings.Contains(text, "request")
}

// isTokenQuota reports whether a quota key appears token-based.
func isTokenQuota(key quotaKey) bool {
	text := strings.ToLower(key.QuotaMetric + " " + key.LimitName)
	return strings.Contains(text, "token")
}

// snapshotFromUsage maps Vertex AI quota usage to remaining-percent metrics.
func snapshotFromUsage(usage quotaUsage) providers.Snapshot {
	now := providerutil.NowString()
	metrics := []providers.MetricValue{
		providerutil.PercentRemainingMetric(
			"session-percent",
			"REQUESTS",
			"Vertex AI request quota remaining",
			usage.RequestsUsedPercent,
			nil,
			"Current quota",
			now,
		),
	}
	if usage.TokensUsedPercent != nil {
		metrics = append(metrics, providerutil.PercentRemainingMetric(
			"weekly-percent",
			"TOKENS",
			"Vertex AI token quota remaining",
			*usage.TokensUsedPercent,
			nil,
			"Current quota",
			now,
		))
	}
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "oauth",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// parseTokenExpiry parses gcloud's ISO token_expiry field.
func parseTokenExpiry(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// emailFromIDToken extracts email from a Google ID token.
func emailFromIDToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(payload + strings.Repeat("=", (4-len(payload)%4)%4))
	}
	if err != nil {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return ""
	}
	return providerutil.StringValue(root["email"])
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// errorSnapshot returns a Vertex AI setup or auth failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: providerName,
		Source:       "oauth",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the Vertex AI provider with the package registry.
func init() {
	providers.Register(Provider{})
}
