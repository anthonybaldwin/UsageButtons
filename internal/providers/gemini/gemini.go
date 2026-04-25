// Package gemini implements the Gemini CLI OAuth usage provider.
//
// Auth: Gemini CLI OAuth credentials from ~/.gemini/oauth_creds.json.
// Endpoints: Google Cloud Code private quota APIs used by Gemini CLI.
package gemini

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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	providerID       = "gemini"
	providerName     = "Gemini"
	defaultTimeout   = 20 * time.Second
	credentialsFile  = "oauth_creds.json"
	settingsFile     = "settings.json"
	quotaEndpoint    = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	codeAssistURL    = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	projectsEndpoint = "https://cloudresourcemanager.googleapis.com/v1/projects"
	tokenEndpoint    = "https://oauth2.googleapis.com/token"
)

var (
	errNotLoggedIn        = errors.New("not logged in to Gemini. Run gemini in a terminal to authenticate with Google")
	errOAuthConfigMissing = errors.New("could not find Gemini CLI OAuth configuration")
	oauthClientIDRe       = regexp.MustCompile(`OAUTH_CLIENT_ID\s*=\s*['"]([\w\-.]+)['"]\s*;?`)
	oauthClientSecretRe   = regexp.MustCompile(`OAUTH_CLIENT_SECRET\s*=\s*['"]([\w\-]+)['"]\s*;?`)
	jsImportRes           = []*regexp.Regexp{
		regexp.MustCompile(`(?:import|export)\s+(?:[^;]*?\s+from\s+)?["'](\./[^"']+\.js)["']`),
		regexp.MustCompile(`import\(\s*["'](\./[^"']+\.js)["']\s*\)`),
	}
)

// Provider fetches Gemini quota data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return providerID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return providerName }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ab87ea" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#1b102a" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "opus-percent"}
}

// Fetch returns the latest Gemini quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	snapshot, err := fetchSnapshot(ctx)
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshot, nil
}

// geminiStatus is the parsed Gemini account and model-quota state.
type geminiStatus struct {
	Quotas []modelQuota
	Email  string
	Plan   string
}

// modelQuota is the lowest remaining quota bucket for one model.
type modelQuota struct {
	ModelID     string
	PercentLeft float64
	ResetTime   *time.Time
}

// oauthCredentials mirrors ~/.gemini/oauth_creds.json.
type oauthCredentials struct {
	AccessToken  string
	IDToken      string
	RefreshToken string
	ExpiryDate   *time.Time
}

// oauthClientCredentials are embedded in the installed Gemini CLI package.
type oauthClientCredentials struct {
	ClientID     string
	ClientSecret string
}

// tokenClaims contains displayable fields from the Google ID token.
type tokenClaims struct {
	Email        string
	HostedDomain string
}

// codeAssistStatus carries optional tier and project data from loadCodeAssist.
type codeAssistStatus struct {
	Tier      string
	ProjectID string
}

// quotaResponse mirrors v1internal:retrieveUserQuota.
type quotaResponse struct {
	Buckets []quotaBucket `json:"buckets"`
}

// quotaBucket is one model/token-type quota bucket.
type quotaBucket struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
	ModelID           string   `json:"modelId"`
}

// projectListResponse mirrors Cloud Resource Manager project listing.
type projectListResponse struct {
	Projects []projectEntry `json:"projects"`
}

// projectEntry is one Cloud Resource Manager project.
type projectEntry struct {
	ProjectID string            `json:"projectId"`
	Labels    map[string]string `json:"labels"`
}

// fetchSnapshot reads local Gemini CLI auth, calls Google quota APIs, and maps
// the response to Stream Deck metrics.
func fetchSnapshot(ctx context.Context) (providers.Snapshot, error) {
	configDir, err := geminiConfigDir()
	if err != nil {
		return providers.Snapshot{}, err
	}
	switch authType := currentAuthType(configDir); authType {
	case "api-key":
		return providers.Snapshot{}, fmt.Errorf("Gemini API key auth is not supported here. Run gemini and choose Google account OAuth")
	case "vertex-ai":
		return providers.Snapshot{}, fmt.Errorf("Gemini Vertex AI auth is not supported by this provider. Use the Vertex AI provider instead")
	}

	creds, rawCreds, credsPath, err := loadCredentials(configDir)
	if err != nil {
		return providers.Snapshot{}, err
	}
	if creds.AccessToken == "" {
		return providers.Snapshot{}, errNotLoggedIn
	}

	accessToken := creds.AccessToken
	idToken := creds.IDToken
	if creds.ExpiryDate != nil && time.Now().After(creds.ExpiryDate.Add(-30*time.Second)) {
		if creds.RefreshToken == "" {
			return providers.Snapshot{}, errNotLoggedIn
		}
		refreshed, refreshedIDToken, err := refreshAccessToken(ctx, creds.RefreshToken, credsPath, rawCreds)
		if err != nil {
			return providers.Snapshot{}, err
		}
		accessToken = refreshed
		if refreshedIDToken != "" {
			idToken = refreshedIDToken
		}
	}

	claims := extractClaimsFromToken(idToken)
	codeAssist := loadCodeAssistStatus(ctx, accessToken)
	projectID := codeAssist.ProjectID
	if projectID == "" {
		projectID = discoverGeminiProjectID(ctx, accessToken)
	}

	quotas, err := fetchQuotas(ctx, accessToken, projectID)
	if err != nil {
		return providers.Snapshot{}, err
	}
	return snapshotFromStatus(geminiStatus{
		Quotas: quotas,
		Email:  claims.Email,
		Plan:   planLabel(codeAssist.Tier, claims.HostedDomain),
	}), nil
}

// geminiConfigDir returns the directory where Gemini CLI stores config.
func geminiConfigDir() (string, error) {
	for _, name := range []string{"GEMINI_CONFIG_DIR", "GEMINI_CONFIG_HOME"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return filepath.Clean(v), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".gemini"), nil
}

// currentAuthType reads Gemini CLI's selected auth type; unknown means "try OAuth".
func currentAuthType(configDir string) string {
	data, err := os.ReadFile(filepath.Join(configDir, settingsFile))
	if err != nil {
		return ""
	}
	var root struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(root.Security.Auth.SelectedType))
}

// loadCredentials reads Gemini CLI OAuth credentials and preserves the raw JSON
// object so refreshed tokens can be written back without discarding extra keys.
func loadCredentials(configDir string) (oauthCredentials, map[string]any, string, error) {
	path := filepath.Join(configDir, credentialsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return oauthCredentials{}, nil, path, errNotLoggedIn
	}
	if err != nil {
		return oauthCredentials{}, nil, path, fmt.Errorf("read Gemini credentials: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return oauthCredentials{}, nil, path, fmt.Errorf("parse Gemini credentials: %w", err)
	}
	var expiry *time.Time
	if n, ok := providerutil.FloatValue(raw["expiry_date"]); ok && n > 0 {
		t := time.UnixMilli(int64(n))
		expiry = &t
	}
	return oauthCredentials{
		AccessToken:  providerutil.StringValue(raw["access_token"]),
		IDToken:      providerutil.StringValue(raw["id_token"]),
		RefreshToken: providerutil.StringValue(raw["refresh_token"]),
		ExpiryDate:   expiry,
	}, raw, path, nil
}

// refreshAccessToken exchanges the Gemini CLI refresh token for a new access
// token and updates oauth_creds.json when the file is writable.
func refreshAccessToken(ctx context.Context, refreshToken, credsPath string, rawCreds map[string]any) (string, string, error) {
	oauthCreds, err := extractOAuthClientCredentials()
	if err != nil {
		return "", "", err
	}
	form := url.Values{}
	form.Set("client_id", oauthCreds.ClientID)
	form.Set("client_secret", oauthCreds.ClientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("build Gemini token refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("refresh Gemini token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", &httputil.Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(body),
			URL:        tokenEndpoint,
			Headers:    resp.Header,
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("parse Gemini token refresh: %w", err)
	}
	accessToken := providerutil.StringValue(parsed["access_token"])
	if accessToken == "" {
		return "", "", fmt.Errorf("Gemini token refresh response missing access token")
	}
	idToken := providerutil.StringValue(parsed["id_token"])
	updateStoredCredentials(credsPath, rawCreds, parsed)
	return accessToken, idToken, nil
}

// updateStoredCredentials persists refreshed OAuth fields best-effort.
func updateStoredCredentials(path string, rawCreds, refresh map[string]any) {
	if rawCreds == nil {
		return
	}
	for _, key := range []string{"access_token", "id_token"} {
		if v := providerutil.StringValue(refresh[key]); v != "" {
			rawCreds[key] = v
		}
	}
	if expiresIn, ok := providerutil.FloatValue(refresh["expires_in"]); ok && expiresIn > 0 {
		rawCreds["expiry_date"] = float64(time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli())
	}
	data, err := json.MarshalIndent(rawCreds, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0600)
}

// extractOAuthClientCredentials locates the installed Gemini CLI package and
// extracts the public OAuth client ID/secret it uses for token refresh.
func extractOAuthClientCredentials() (oauthClientCredentials, error) {
	envID := strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_ID"))
	envSecret := strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_SECRET"))
	if envID != "" && envSecret != "" {
		return oauthClientCredentials{ClientID: envID, ClientSecret: envSecret}, nil
	}

	geminiPath := geminiBinaryPath()
	if geminiPath == "" {
		return oauthClientCredentials{}, errOAuthConfigMissing
	}
	resolved := geminiPath
	if realPath, err := filepath.EvalSymlinks(geminiPath); err == nil && realPath != "" {
		resolved = realPath
	}
	if creds, ok := extractOAuthCredentialsFromLegacyPaths(resolved); ok {
		return creds, nil
	}
	if root := findGeminiPackageRoot(resolved); root != "" {
		if creds, ok := extractOAuthCredentialsFromPackageRoot(root); ok {
			return creds, nil
		}
	}
	return oauthClientCredentials{}, errOAuthConfigMissing
}

// geminiBinaryPath finds the Gemini CLI executable across common launchers.
func geminiBinaryPath() string {
	candidates := []string{"gemini"}
	if filepath.Separator == '\\' {
		candidates = append(candidates, "gemini.cmd", "gemini.exe", "gemini.ps1")
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// extractOAuthCredentialsFromLegacyPaths checks common npm, Homebrew, Bun, and
// Nix layouts before falling back to package-root discovery.
func extractOAuthCredentialsFromLegacyPaths(realGeminiPath string) (oauthClientCredentials, bool) {
	binDir := filepath.Dir(realGeminiPath)
	baseDir := filepath.Dir(binDir)
	oauthFile := filepath.Join("dist", "src", "code_assist", "oauth2.js")
	oauthSubpath := filepath.Join("node_modules", "@google", "gemini-cli", "node_modules", "@google", "gemini-cli-core", oauthFile)
	candidates := []string{
		filepath.Join(baseDir, "libexec", "lib", oauthSubpath),
		filepath.Join(baseDir, "lib", oauthSubpath),
		filepath.Join(baseDir, "share", "gemini-cli", "node_modules", "@google", "gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "..", "gemini-cli-core", oauthFile),
		filepath.Join(baseDir, "node_modules", "@google", "gemini-cli-core", oauthFile),
	}
	for _, path := range candidates {
		if creds, ok := parseOAuthCredentialsFile(path); ok {
			return creds, true
		}
	}
	return oauthClientCredentials{}, false
}

// findGeminiPackageRoot walks upward from the resolved CLI path and validates
// package.json name to avoid picking an unrelated package.
func findGeminiPackageRoot(start string) string {
	current := filepath.Clean(start)
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for range 9 {
		if isGeminiPackageRoot(current) {
			return current
		}
		globalRoot := filepath.Join(current, "lib", "node_modules", "@google", "gemini-cli")
		if isGeminiPackageRoot(globalRoot) {
			return globalRoot
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
	return ""
}

// isGeminiPackageRoot checks package.json for @google/gemini-cli.
func isGeminiPackageRoot(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(data, &pkg) == nil && pkg.Name == "@google/gemini-cli"
}

// extractOAuthCredentialsFromPackageRoot checks current Gemini CLI package
// layouts, including bundled JavaScript entrypoints.
func extractOAuthCredentialsFromPackageRoot(root string) (oauthClientCredentials, bool) {
	oauthFile := filepath.Join("dist", "src", "code_assist", "oauth2.js")
	for _, path := range []string{
		filepath.Join(root, oauthFile),
		filepath.Join(root, "node_modules", "@google", "gemini-cli-core", oauthFile),
	} {
		if creds, ok := parseOAuthCredentialsFile(path); ok {
			return creds, true
		}
	}
	return extractOAuthCredentialsFromBundle(root)
}

// extractOAuthCredentialsFromBundle follows same-bundle JS imports looking for
// OAuth constants in newer single-file Gemini CLI distributions.
func extractOAuthCredentialsFromBundle(root string) (oauthClientCredentials, bool) {
	bundleRoot := filepath.Join(root, "bundle")
	entry := filepath.Join(bundleRoot, "gemini.js")
	if _, err := os.Stat(entry); err != nil {
		return oauthClientCredentials{}, false
	}
	queue := []string{entry}
	seen := map[string]bool{}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		clean := filepath.Clean(path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		content, err := os.ReadFile(clean)
		if err != nil {
			continue
		}
		if creds, ok := parseOAuthCredentials(string(content)); ok {
			return creds, true
		}
		for _, importPath := range relativeJSImports(string(content)) {
			next := filepath.Clean(filepath.Join(filepath.Dir(clean), importPath))
			if strings.HasPrefix(next, filepath.Clean(bundleRoot)+string(filepath.Separator)) {
				queue = append(queue, next)
			}
		}
	}
	return oauthClientCredentials{}, false
}

// parseOAuthCredentialsFile reads and parses a JavaScript OAuth config file.
func parseOAuthCredentialsFile(path string) (oauthClientCredentials, bool) {
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return oauthClientCredentials{}, false
	}
	return parseOAuthCredentials(string(content))
}

// parseOAuthCredentials extracts OAUTH_CLIENT_ID and OAUTH_CLIENT_SECRET.
func parseOAuthCredentials(content string) (oauthClientCredentials, bool) {
	idMatch := oauthClientIDRe.FindStringSubmatch(content)
	secretMatch := oauthClientSecretRe.FindStringSubmatch(content)
	if len(idMatch) < 2 || len(secretMatch) < 2 {
		return oauthClientCredentials{}, false
	}
	return oauthClientCredentials{ClientID: idMatch[1], ClientSecret: secretMatch[1]}, true
}

// relativeJSImports extracts bundle-local JavaScript imports.
func relativeJSImports(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range jsImportRes {
		for _, match := range re.FindAllStringSubmatch(content, -1) {
			if len(match) < 2 || seen[match[1]] {
				continue
			}
			seen[match[1]] = true
			out = append(out, match[1])
		}
	}
	return out
}

// loadCodeAssistStatus reads the account tier and managed project ID.
func loadCodeAssistStatus(ctx context.Context, accessToken string) codeAssistStatus {
	var root map[string]any
	err := postGoogleJSON(ctx, codeAssistURL, accessToken, map[string]any{
		"metadata": map[string]string{
			"ideType":    "GEMINI_CLI",
			"pluginType": "GEMINI",
		},
	}, &root)
	if err != nil {
		return codeAssistStatus{}
	}
	var status codeAssistStatus
	if tier, ok := providerutil.NestedMap(root, "currentTier"); ok {
		status.Tier = providerutil.FirstString(tier, "id")
	}
	switch project := root["cloudaicompanionProject"].(type) {
	case string:
		status.ProjectID = strings.TrimSpace(project)
	case map[string]any:
		status.ProjectID = providerutil.FirstString(project, "id", "projectId")
	}
	return status
}

// discoverGeminiProjectID finds a likely Gemini CLI quota project.
func discoverGeminiProjectID(ctx context.Context, accessToken string) string {
	var projects projectListResponse
	if err := getGoogleJSON(ctx, projectsEndpoint, accessToken, &projects); err != nil {
		return ""
	}
	for _, project := range projects.Projects {
		if strings.HasPrefix(project.ProjectID, "gen-lang-client") {
			return project.ProjectID
		}
		if _, ok := project.Labels["generative-language"]; ok {
			return project.ProjectID
		}
	}
	return ""
}

// fetchQuotas calls Gemini's quota API and returns one lowest bucket per model.
func fetchQuotas(ctx context.Context, accessToken, projectID string) ([]modelQuota, error) {
	body := map[string]string{}
	if projectID != "" {
		body["project"] = projectID
	}
	var resp quotaResponse
	if err := postGoogleJSON(ctx, quotaEndpoint, accessToken, body, &resp); err != nil {
		return nil, err
	}
	return parseQuotaResponse(resp)
}

// parseQuotaResponse keeps the lowest remaining fraction for each model.
func parseQuotaResponse(resp quotaResponse) ([]modelQuota, error) {
	if len(resp.Buckets) == 0 {
		return nil, fmt.Errorf("Gemini response missing quota buckets")
	}
	byModel := map[string]modelQuota{}
	for _, bucket := range resp.Buckets {
		if bucket.ModelID == "" || bucket.RemainingFraction == nil {
			continue
		}
		percent := math.Max(0, math.Min(100, *bucket.RemainingFraction*100))
		reset := parseResetTime(bucket.ResetTime)
		quota := modelQuota{ModelID: bucket.ModelID, PercentLeft: percent, ResetTime: reset}
		existing, ok := byModel[bucket.ModelID]
		if !ok || quota.PercentLeft < existing.PercentLeft {
			byModel[bucket.ModelID] = quota
		}
	}
	if len(byModel) == 0 {
		return nil, fmt.Errorf("Gemini response missing model quota data")
	}
	out := make([]modelQuota, 0, len(byModel))
	for _, quota := range byModel {
		out = append(out, quota)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out, nil
}

// getGoogleJSON performs an authenticated Google GET and decodes JSON.
func getGoogleJSON(ctx context.Context, endpoint, accessToken string, dst any) error {
	return googleJSON(ctx, http.MethodGet, endpoint, accessToken, nil, dst)
}

// postGoogleJSON performs an authenticated Google POST and decodes JSON.
func postGoogleJSON(ctx context.Context, endpoint, accessToken string, payload, dst any) error {
	return googleJSON(ctx, http.MethodPost, endpoint, accessToken, payload, dst)
}

// googleJSON performs one authenticated Google JSON request.
func googleJSON(ctx context.Context, method, endpoint, accessToken string, payload, dst any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal Gemini request: %w", err)
		}
		body = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("build Gemini request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httputil.DefaultUserAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return errNotLoggedIn
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httputil.Error{
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       string(data),
			URL:        endpoint,
			Headers:    resp.Header,
		}
	}
	if dst != nil {
		if err := json.Unmarshal(data, dst); err != nil {
			return fmt.Errorf("parse Gemini response: %w", err)
		}
	}
	return nil
}

// snapshotFromStatus maps Gemini quotas to Pro, Flash, and Flash Lite metrics.
func snapshotFromStatus(status geminiStatus) providers.Snapshot {
	now := providerutil.NowString()
	var metrics []providers.MetricValue
	if q, ok := lowestMatchingQuota(status.Quotas, isProModel); ok {
		metrics = append(metrics, quotaMetric("session-percent", "PRO", "Gemini Pro quota remaining", q, now))
	}
	if q, ok := lowestMatchingQuota(status.Quotas, isFlashModel); ok {
		metrics = append(metrics, quotaMetric("weekly-percent", "FLASH", "Gemini Flash quota remaining", q, now))
	}
	if q, ok := lowestMatchingQuota(status.Quotas, isFlashLiteModel); ok {
		metrics = append(metrics, quotaMetric("opus-percent", "FLASH LITE", "Gemini Flash Lite quota remaining", q, now))
	}
	if len(metrics) == 0 && len(status.Quotas) > 0 {
		q := status.Quotas[0]
		metrics = append(metrics, quotaMetric("session-percent", "QUOTA", "Gemini quota remaining", q, now))
	}

	name := providerName
	if status.Plan != "" {
		name += " " + status.Plan
	}
	return providers.Snapshot{
		ProviderID:   providerID,
		ProviderName: name,
		Source:       "oauth",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// quotaMetric turns one model quota into a remaining-percent metric.
func quotaMetric(id, label, name string, quota modelQuota, now string) providers.MetricValue {
	usedPct := 100 - quota.PercentLeft
	return providerutil.PercentRemainingMetric(id, label, name, usedPct, quota.ResetTime, quota.ModelID, now)
}

// lowestMatchingQuota returns the lowest remaining quota accepted by match.
func lowestMatchingQuota(quotas []modelQuota, match func(string) bool) (modelQuota, bool) {
	var picked modelQuota
	found := false
	for _, quota := range quotas {
		if !match(strings.ToLower(quota.ModelID)) {
			continue
		}
		if !found || quota.PercentLeft < picked.PercentLeft {
			picked = quota
			found = true
		}
	}
	return picked, found
}

// isFlashLiteModel reports whether a model is in the Flash Lite quota lane.
func isFlashLiteModel(id string) bool {
	return strings.Contains(id, "flash-lite")
}

// isFlashModel reports whether a model is in the Flash quota lane.
func isFlashModel(id string) bool {
	return strings.Contains(id, "flash") && !isFlashLiteModel(id)
}

// isProModel reports whether a model is in the Pro quota lane.
func isProModel(id string) bool {
	return strings.Contains(id, "pro")
}

// planLabel maps Cloud Code tier IDs to CodexBar-compatible display labels.
func planLabel(tier, hostedDomain string) string {
	switch tier {
	case "standard-tier":
		return "Paid"
	case "free-tier":
		if hostedDomain != "" {
			return "Workspace"
		}
		return "Free"
	case "legacy-tier":
		return "Legacy"
	default:
		return ""
	}
}

// extractClaimsFromToken decodes the middle segment of a Google ID token.
func extractClaimsFromToken(idToken string) tokenClaims {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return tokenClaims{}
	}
	payload := parts[1]
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(payload + strings.Repeat("=", (4-len(payload)%4)%4))
	}
	if err != nil {
		return tokenClaims{}
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return tokenClaims{}
	}
	return tokenClaims{
		Email:        providerutil.StringValue(root["email"]),
		HostedDomain: providerutil.StringValue(root["hd"]),
	}
}

// parseResetTime converts Gemini ISO timestamps to UTC times.
func parseResetTime(raw string) *time.Time {
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

// errorSnapshot returns a configured-but-unavailable Gemini snapshot.
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

// init registers the Gemini provider with the package registry.
func init() {
	providers.Register(Provider{})
}
