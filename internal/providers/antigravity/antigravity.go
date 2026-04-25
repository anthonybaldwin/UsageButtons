// Package antigravity implements the Antigravity local language-server quota provider.
//
// Auth: none entered in the plugin. The provider talks to the running
// Antigravity language server on localhost and uses the CSRF token from the
// server process command line.
package antigravity

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const (
	getUserStatusPath         = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
	commandModelConfigPath    = "/exa.language_server_pb.LanguageServerService/GetCommandModelConfigs"
	unleashPath               = "/exa.language_server_pb.LanguageServerService/GetUnleashData"
	defaultRequestTimeout     = 8 * time.Second
	processDiscoveryTimeout   = 4 * time.Second
	antigravityProviderID     = "antigravity"
	antigravityProviderName   = "Antigravity"
	antigravityLanguageServer = "language_server"
)

var lsofPortRe = regexp.MustCompile(`:(\d+)\s+\(LISTEN\)`)

// processInfo describes a detected Antigravity language-server process.
type processInfo struct {
	PID                      int
	CSRFToken                string
	ExtensionPort            int
	ExtensionServerCSRFToken string
	CommandLine              string
	ExplicitPorts            []int
}

// endpoint is one localhost Antigravity API target.
type endpoint struct {
	Scheme    string
	Port      int
	CSRFToken string
	Source    string
}

// modelQuota is one Antigravity model quota lane.
type modelQuota struct {
	Label             string
	ModelID           string
	RemainingFraction *float64
	ResetTime         *time.Time
}

// usageSnapshot is the normalized Antigravity account and quota state.
type usageSnapshot struct {
	ModelQuotas  []modelQuota
	AccountPlan  string
	AccountEmail string
	UpdatedAt    time.Time
}

// normalizedModel is a quota annotated with its display family.
type normalizedModel struct {
	Quota             modelQuota
	Family            modelFamily
	SelectionPriority *int
}

// modelFamily groups Antigravity models into the three CodexBar display lanes.
type modelFamily int

const (
	familyUnknown modelFamily = iota
	familyClaude
	familyGeminiPro
	familyGeminiFlash
)

// Provider fetches Antigravity usage data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return antigravityProviderID }

// Name returns the human-readable provider name.
func (Provider) Name() string { return antigravityProviderName }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#60ba7e" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#0d2418" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent", "weekly-percent", "opus-percent"}
}

// Fetch returns the latest Antigravity quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	usage, err := fetchUsage()
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsage discovers Antigravity's localhost API and fetches quota data.
func fetchUsage() (usageSnapshot, error) {
	info, err := detectProcessInfo()
	if err != nil {
		return usageSnapshot{}, err
	}
	ports, err := listeningPorts(info)
	if err != nil {
		return usageSnapshot{}, err
	}
	candidates := requestEndpoints(info, ports)
	if len(candidates) == 0 {
		return usageSnapshot{}, errors.New("Antigravity is running but no API ports were found. Try again in a few seconds.")
	}
	candidates = prioritizeReachableEndpoints(candidates)

	usage, err := makeParsedRequest(candidates, getUserStatusPath, defaultRequestBody(), parseUserStatusResponse)
	if err != nil {
		usage, err = makeParsedRequest(candidates, commandModelConfigPath, defaultRequestBody(), parseCommandModelResponse)
	}
	if err != nil {
		return usageSnapshot{}, err
	}
	usage.UpdatedAt = time.Now().UTC()
	return usage, nil
}

// detectProcessInfo locates the running Antigravity language server.
func detectProcessInfo() (processInfo, error) {
	if info, ok := processInfoFromEnv(); ok {
		return info, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), processDiscoveryTimeout)
	defer cancel()
	if runtime.GOOS == "windows" {
		return detectWindowsProcess(ctx)
	}
	return detectUnixProcess(ctx)
}

// processInfoFromEnv returns an explicit localhost target for diagnostics.
func processInfoFromEnv() (processInfo, bool) {
	token := firstEnv("ANTIGRAVITY_CSRF_TOKEN", "ANTIGRAVITY_LANGUAGE_SERVER_CSRF_TOKEN")
	ports := portsFromEnv("ANTIGRAVITY_PORT", "ANTIGRAVITY_LANGUAGE_SERVER_PORT")
	extensionPort := firstEnvInt("ANTIGRAVITY_EXTENSION_SERVER_PORT")
	if token == "" || (len(ports) == 0 && extensionPort <= 0) {
		return processInfo{}, false
	}
	return processInfo{
		CSRFToken:                token,
		ExtensionPort:            extensionPort,
		ExtensionServerCSRFToken: firstEnv("ANTIGRAVITY_EXTENSION_SERVER_CSRF_TOKEN"),
		CommandLine:              "ANTIGRAVITY_* environment override",
		ExplicitPorts:            ports,
	}, true
}

// detectUnixProcess scans ps output for Antigravity's language server.
func detectUnixProcess(ctx context.Context) (processInfo, error) {
	result, err := providerutil.RunCommand(ctx, "ps", "-ax", "-o", "pid=,command=")
	if err != nil {
		return processInfo{}, fmt.Errorf("Antigravity process scan failed: %w", err)
	}
	rows := []processRow{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if row, ok := parsePSLine(line); ok {
			rows = append(rows, row)
		}
	}
	return processInfoFromRows(rows)
}

// detectWindowsProcess scans WMI/CIM output for Antigravity's language server.
func detectWindowsProcess(ctx context.Context) (processInfo, error) {
	command := "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -and $_.CommandLine.ToLower().Contains('language_server') -and $_.CommandLine.ToLower().Contains('antigravity') } | Select-Object ProcessId,CommandLine | ConvertTo-Json -Compress"
	for _, bin := range []string{"powershell", "powershell.exe", "pwsh", "pwsh.exe"} {
		result, err := providerutil.RunCommand(ctx, bin, "-NoProfile", "-Command", command)
		if err != nil || strings.TrimSpace(result.Stdout) == "" {
			continue
		}
		rows, parseErr := parseWindowsProcesses(result.Stdout)
		if parseErr != nil {
			continue
		}
		return processInfoFromRows(rows)
	}
	return processInfo{}, errors.New("Antigravity language server not detected. Launch Antigravity and retry.")
}

// processRow is one process-list row.
type processRow struct {
	PID         int
	CommandLine string
}

// parsePSLine converts a ps output line into a process row.
func parsePSLine(line string) (processRow, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return processRow{}, false
	}
	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) != 2 {
		return processRow{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return processRow{}, false
	}
	return processRow{PID: pid, CommandLine: strings.TrimSpace(parts[1])}, true
}

// parseWindowsProcesses decodes ConvertTo-Json output from the process scan.
func parseWindowsProcesses(text string) ([]processRow, error) {
	type winProcess struct {
		ProcessID   int    `json:"ProcessId"`
		CommandLine string `json:"CommandLine"`
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	var one winProcess
	if err := json.Unmarshal([]byte(trimmed), &one); err == nil && one.CommandLine != "" {
		return []processRow{{PID: one.ProcessID, CommandLine: one.CommandLine}}, nil
	}
	var many []winProcess
	if err := json.Unmarshal([]byte(trimmed), &many); err != nil {
		return nil, err
	}
	rows := make([]processRow, 0, len(many))
	for _, item := range many {
		if item.CommandLine != "" {
			rows = append(rows, processRow{PID: item.ProcessID, CommandLine: item.CommandLine})
		}
	}
	return rows, nil
}

// processInfoFromRows extracts tokens from candidate process rows.
func processInfoFromRows(rows []processRow) (processInfo, error) {
	sawAntigravity := false
	for _, row := range rows {
		lower := strings.ToLower(row.CommandLine)
		if !strings.Contains(lower, antigravityLanguageServer) || !isAntigravityCommandLine(lower) {
			continue
		}
		sawAntigravity = true
		token := extractFlag("--csrf_token", row.CommandLine)
		if token == "" {
			continue
		}
		return processInfo{
			PID:                      row.PID,
			CSRFToken:                token,
			ExtensionPort:            extractFlagInt("--extension_server_port", row.CommandLine),
			ExtensionServerCSRFToken: extractFlag("--extension_server_csrf_token", row.CommandLine),
			CommandLine:              row.CommandLine,
			ExplicitPorts:            portsFromEnv("ANTIGRAVITY_PORT", "ANTIGRAVITY_LANGUAGE_SERVER_PORT"),
		}, nil
	}
	if sawAntigravity {
		return processInfo{}, errors.New("Antigravity CSRF token not found. Restart Antigravity and retry.")
	}
	return processInfo{}, errors.New("Antigravity language server not detected. Launch Antigravity and retry.")
}

// isAntigravityCommandLine reports whether a language-server command belongs to Antigravity.
func isAntigravityCommandLine(command string) bool {
	return strings.Contains(command, "--app_data_dir") && strings.Contains(command, "antigravity") ||
		strings.Contains(command, "/antigravity/") ||
		strings.Contains(command, `\antigravity\`)
}

// extractFlag returns a command-line flag value in --flag value or --flag=value form.
func extractFlag(flag string, command string) string {
	fields := splitCommandLine(command)
	for i, field := range fields {
		if strings.EqualFold(field, flag) && i+1 < len(fields) {
			return strings.TrimSpace(fields[i+1])
		}
		prefix := flag + "="
		if strings.HasPrefix(strings.ToLower(field), strings.ToLower(prefix)) {
			return strings.TrimSpace(field[len(prefix):])
		}
	}
	return ""
}

// extractFlagInt returns an integer command-line flag value.
func extractFlagInt(flag string, command string) int {
	value, _ := strconv.Atoi(extractFlag(flag, command))
	return value
}

// splitCommandLine splits enough shell quoting to read Antigravity flags.
func splitCommandLine(command string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range command {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && quote != 0:
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

// listeningPorts returns the local ports owned by the Antigravity process.
func listeningPorts(info processInfo) ([]int, error) {
	ports := append([]int{}, info.ExplicitPorts...)
	if info.PID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), processDiscoveryTimeout)
		defer cancel()
		var detected []int
		var err error
		if runtime.GOOS == "windows" {
			detected, err = windowsListeningPorts(ctx, info.PID)
		} else {
			detected, err = unixListeningPorts(ctx, info.PID)
		}
		if err == nil {
			ports = append(ports, detected...)
		}
	}
	ports = uniqueInts(ports)
	if len(ports) == 0 && info.ExtensionPort <= 0 {
		return nil, errors.New("Antigravity is running but not exposing ports yet. Try again in a few seconds.")
	}
	return ports, nil
}

// unixListeningPorts asks lsof for listening TCP ports owned by pid.
func unixListeningPorts(ctx context.Context, pid int) ([]int, error) {
	result, err := providerutil.RunCommand(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}
	matches := lsofPortRe.FindAllStringSubmatch(result.Stdout, -1)
	ports := make([]int, 0, len(matches))
	for _, match := range matches {
		port, err := strconv.Atoi(match[1])
		if err == nil {
			ports = append(ports, port)
		}
	}
	return uniqueInts(ports), nil
}

// windowsListeningPorts asks netstat for listening TCP ports owned by pid.
func windowsListeningPorts(ctx context.Context, pid int) ([]int, error) {
	result, err := providerutil.RunCommand(ctx, "netstat", "-ano", "-p", "tcp")
	if err != nil {
		return nil, err
	}
	pidText := strconv.Itoa(pid)
	var ports []int
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		if !strings.EqualFold(fields[len(fields)-2], "LISTENING") || fields[len(fields)-1] != pidText {
			continue
		}
		if port, ok := portFromAddress(fields[1]); ok {
			ports = append(ports, port)
		}
	}
	return uniqueInts(ports), nil
}

// portFromAddress extracts a port from netstat local-address text.
func portFromAddress(address string) (int, bool) {
	address = strings.TrimSpace(address)
	idx := strings.LastIndex(address, ":")
	if idx < 0 || idx == len(address)-1 {
		return 0, false
	}
	raw := strings.Trim(address[idx+1:], "[]")
	port, err := strconv.Atoi(raw)
	return port, err == nil
}

// requestEndpoints builds API targets in the same priority CodexBar uses.
func requestEndpoints(info processInfo, ports []int) []endpoint {
	var endpoints []endpoint
	for _, port := range ports {
		endpoints = append(endpoints, endpoint{
			Scheme:    "https",
			Port:      port,
			CSRFToken: info.CSRFToken,
			Source:    "language-server",
		})
	}
	if info.ExtensionPort > 0 {
		if info.ExtensionServerCSRFToken != "" {
			endpoints = append(endpoints, endpoint{
				Scheme:    "http",
				Port:      info.ExtensionPort,
				CSRFToken: info.ExtensionServerCSRFToken,
				Source:    "extension-server",
			})
		}
		if info.ExtensionServerCSRFToken != info.CSRFToken {
			endpoints = append(endpoints, endpoint{
				Scheme:    "http",
				Port:      info.ExtensionPort,
				CSRFToken: info.CSRFToken,
				Source:    "extension-server",
			})
		}
	}
	return uniqueEndpoints(endpoints)
}

// prioritizeReachableEndpoints moves a successfully probed endpoint to the front.
func prioritizeReachableEndpoints(endpoints []endpoint) []endpoint {
	for i, candidate := range endpoints {
		if probeEndpoint(candidate) {
			out := []endpoint{candidate}
			out = append(out, endpoints[:i]...)
			out = append(out, endpoints[i+1:]...)
			return uniqueEndpoints(out)
		}
	}
	return endpoints
}

// probeEndpoint checks whether one endpoint responds to the Unleash method.
func probeEndpoint(endpoint endpoint) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := sendRequest(ctx, endpoint, unleashPath, unleashRequestBody())
	return err == nil
}

// makeParsedRequest sends one request to each endpoint until parsing succeeds.
func makeParsedRequest[T any](
	endpoints []endpoint,
	path string,
	body map[string]any,
	parse func([]byte) (T, error),
) (T, error) {
	var zero T
	var lastErr error
	for _, candidate := range endpoints {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		data, err := sendRequest(ctx, candidate, path, body)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		parsed, err := parse(data)
		if err == nil {
			return parsed, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("Antigravity API request failed")
	}
	return zero, lastErr
}

// sendRequest posts JSON to one Antigravity endpoint.
func sendRequest(ctx context.Context, endpoint endpoint, path string, body map[string]any) ([]byte, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s://127.0.0.1:%d%s", endpoint.Scheme, endpoint.Port, path),
		bytes.NewReader(rawBody),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", strconv.Itoa(len(rawBody)))
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Codeium-Csrf-Token", endpoint.CSRFToken)

	client := httpClientFor(endpoint)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Antigravity API error: HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// httpClientFor returns a short-lived localhost client for one endpoint.
func httpClientFor(endpoint endpoint) *http.Client {
	transport := &http.Transport{}
	if endpoint.Scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   defaultRequestTimeout,
		Transport: transport,
	}
}

// defaultRequestBody builds the Connect request body Antigravity accepts.
func defaultRequestBody() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"ideName":       "antigravity",
			"extensionName": "antigravity",
			"ideVersion":    "unknown",
			"locale":        "en",
		},
	}
}

// unleashRequestBody builds the probe request body.
func unleashRequestBody() map[string]any {
	return map[string]any{
		"context": map[string]any{
			"properties": map[string]any{
				"devMode":                 "false",
				"extensionVersion":        "unknown",
				"hasAnthropicModelAccess": "true",
				"ide":                     "antigravity",
				"ideVersion":              "unknown",
				"installationId":          "usage-buttons",
				"language":                "UNSPECIFIED",
				"os":                      runtime.GOOS,
				"requestedModelId":        "MODEL_UNSPECIFIED",
			},
		},
	}
}

// parseUserStatusResponse decodes the preferred Antigravity status payload.
func parseUserStatusResponse(data []byte) (usageSnapshot, error) {
	var response userStatusResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return usageSnapshot{}, err
	}
	if invalid := invalidCode(response.Code); invalid != "" {
		return usageSnapshot{}, fmt.Errorf("Antigravity API error: %s", invalid)
	}
	if response.UserStatus == nil {
		return usageSnapshot{}, errors.New("Could not parse Antigravity quota: missing userStatus")
	}
	models := quotasFromConfigs(response.UserStatus.CascadeModelConfigData.ClientModelConfigs)
	if len(models) == 0 {
		return usageSnapshot{}, errors.New("Could not parse Antigravity quota: no quota models available")
	}
	return usageSnapshot{
		ModelQuotas:  models,
		AccountPlan:  response.UserStatus.PreferredPlanName(),
		AccountEmail: response.UserStatus.Email,
	}, nil
}

// parseCommandModelResponse decodes the fallback model-config payload.
func parseCommandModelResponse(data []byte) (usageSnapshot, error) {
	var response commandModelConfigResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return usageSnapshot{}, err
	}
	if invalid := invalidCode(response.Code); invalid != "" {
		return usageSnapshot{}, fmt.Errorf("Antigravity API error: %s", invalid)
	}
	models := quotasFromConfigs(response.ClientModelConfigs)
	if len(models) == 0 {
		return usageSnapshot{}, errors.New("Could not parse Antigravity quota: no quota models available")
	}
	return usageSnapshot{ModelQuotas: models}, nil
}

// quotasFromConfigs extracts quota rows from Antigravity model configs.
func quotasFromConfigs(configs []modelConfig) []modelQuota {
	var out []modelQuota
	for _, config := range configs {
		if config.QuotaInfo == nil {
			continue
		}
		var resetAt *time.Time
		if t, ok := providerutil.TimeValue(config.QuotaInfo.ResetTime); ok {
			resetAt = &t
		}
		out = append(out, modelQuota{
			Label:             strings.TrimSpace(config.Label),
			ModelID:           strings.TrimSpace(config.ModelOrAlias.Model),
			RemainingFraction: config.QuotaInfo.RemainingFraction,
			ResetTime:         resetAt,
		})
	}
	return out
}

// invalidCode returns a non-empty string when an Antigravity response code failed.
func invalidCode(code *codeValue) string {
	if code == nil || code.IsOK() {
		return ""
	}
	return code.Raw
}

// snapshotFromUsage maps Antigravity quota data into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.Format(time.RFC3339)
	normalized := normalizedModels(usage.ModelQuotas)
	primary := representative(familyClaude, normalized)
	secondary := representative(familyGeminiPro, normalized)
	tertiary := representative(familyGeminiFlash, normalized)
	if primary == nil && secondary == nil && tertiary == nil {
		primary = fallbackRepresentative(normalized)
	}

	var metrics []providers.MetricValue
	if primary != nil {
		metrics = append(metrics, quotaMetric("session-percent", "CLAUDE", "Antigravity Claude quota remaining", *primary, now))
	}
	if secondary != nil {
		metrics = append(metrics, quotaMetric("weekly-percent", "GEMINI PRO", "Antigravity Gemini Pro quota remaining", *secondary, now))
	}
	if tertiary != nil {
		metrics = append(metrics, quotaMetric("opus-percent", "GEMINI FLASH", "Antigravity Gemini Flash quota remaining", *tertiary, now))
	}

	return providers.Snapshot{
		ProviderID:   antigravityProviderID,
		ProviderName: providerName(usage.AccountPlan),
		Source:       "local",
		Metrics:      metrics,
		Status:       "operational",
	}
}

// quotaMetric converts one model quota into a remaining-percent metric.
func quotaMetric(id, label, name string, quota modelQuota, now string) providers.MetricValue {
	remaining := quota.RemainingPercent()
	used := 100 - remaining
	metric := providerutil.PercentRemainingMetric(id, label, name, used, quota.ResetTime, quota.Caption(), now)
	metric.NumericValue = &remaining
	return metric
}

// RemainingPercent returns the model quota remaining as 0..100.
func (q modelQuota) RemainingPercent() float64 {
	if q.RemainingFraction == nil {
		return 0
	}
	return math.Max(0, math.Min(100, *q.RemainingFraction*100))
}

// Caption returns the compact model label shown below the percentage.
func (q modelQuota) Caption() string {
	if q.Label != "" {
		return q.Label
	}
	if q.ModelID != "" {
		return q.ModelID
	}
	return "Quota"
}

// providerName returns Antigravity with a plan suffix when available.
func providerName(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return antigravityProviderName
	}
	return antigravityProviderName + " " + plan
}

// normalizedModels annotates quotas with family selection metadata.
func normalizedModels(models []modelQuota) []normalizedModel {
	out := make([]normalizedModel, 0, len(models))
	for _, quota := range models {
		out = append(out, normalizeModel(quota))
	}
	return out
}

// normalizeModel classifies one quota into a CodexBar-style display lane.
func normalizeModel(quota modelQuota) normalizedModel {
	modelID := strings.ToLower(quota.ModelID)
	label := strings.ToLower(quota.Label)
	family := modelFamilyFor(modelID)
	if family == familyUnknown {
		family = modelFamilyFor(label)
	}
	isLite := strings.Contains(modelID, "lite") || strings.Contains(label, "lite")
	isAutocomplete := strings.Contains(modelID, "autocomplete") ||
		strings.Contains(label, "autocomplete") ||
		strings.HasPrefix(modelID, "tab_")
	isLowPriorityGeminiPro := strings.Contains(modelID, "pro-low") ||
		(strings.Contains(label, "pro") && strings.Contains(label, "low"))

	var priority *int
	switch family {
	case familyClaude:
		priority = intPtr(0)
	case familyGeminiPro:
		switch {
		case isLowPriorityGeminiPro:
			priority = intPtr(0)
		case !isLite && !isAutocomplete:
			priority = intPtr(1)
		}
	case familyGeminiFlash:
		if !isLite && !isAutocomplete {
			priority = intPtr(0)
		}
	}
	return normalizedModel{Quota: quota, Family: family, SelectionPriority: priority}
}

// modelFamilyFor maps model IDs and labels to Antigravity display families.
func modelFamilyFor(text string) modelFamily {
	switch {
	case strings.Contains(text, "claude"):
		return familyClaude
	case strings.Contains(text, "gemini") && strings.Contains(text, "pro"):
		return familyGeminiPro
	case strings.Contains(text, "gemini") && strings.Contains(text, "flash"):
		return familyGeminiFlash
	default:
		return familyUnknown
	}
}

// representative selects the preferred quota for one family.
func representative(family modelFamily, models []normalizedModel) *modelQuota {
	var candidates []normalizedModel
	for _, model := range models {
		if model.Family == family && model.SelectionPriority != nil {
			candidates = append(candidates, model)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if *left.SelectionPriority != *right.SelectionPriority {
			return *left.SelectionPriority < *right.SelectionPriority
		}
		leftHasRemaining := left.Quota.RemainingFraction != nil
		rightHasRemaining := right.Quota.RemainingFraction != nil
		if leftHasRemaining != rightHasRemaining {
			return leftHasRemaining
		}
		return left.Quota.RemainingPercent() < right.Quota.RemainingPercent()
	})
	quota := candidates[0].Quota
	return &quota
}

// fallbackRepresentative selects the most-constrained model when no family matches.
func fallbackRepresentative(models []normalizedModel) *modelQuota {
	if len(models) == 0 {
		return nil
	}
	sort.Slice(models, func(i, j int) bool {
		left := models[i]
		right := models[j]
		leftHasRemaining := left.Quota.RemainingFraction != nil
		rightHasRemaining := right.Quota.RemainingFraction != nil
		if leftHasRemaining != rightHasRemaining {
			return leftHasRemaining
		}
		if left.Quota.RemainingPercent() != right.Quota.RemainingPercent() {
			return left.Quota.RemainingPercent() < right.Quota.RemainingPercent()
		}
		return strings.ToLower(left.Quota.Label) < strings.ToLower(right.Quota.Label)
	})
	quota := models[0].Quota
	return &quota
}

// errorSnapshot returns an Antigravity setup or parse failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   antigravityProviderID,
		ProviderName: antigravityProviderName,
		Source:       "local",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// firstEnv returns the first non-empty environment value.
func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// firstEnvInt returns the first integer environment value.
func firstEnvInt(names ...string) int {
	value, _ := strconv.Atoi(firstEnv(names...))
	return value
}

// portsFromEnv returns comma-separated integer ports from the first non-empty env var.
func portsFromEnv(names ...string) []int {
	raw := firstEnv(names...)
	if raw == "" {
		return nil
	}
	var ports []int
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		port, err := strconv.Atoi(strings.TrimSpace(field))
		if err == nil && port > 0 {
			ports = append(ports, port)
		}
	}
	return uniqueInts(ports)
}

// uniqueInts sorts and deduplicates integers.
func uniqueInts(values []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, value := range values {
		if value <= 0 || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

// uniqueEndpoints removes duplicate request targets.
func uniqueEndpoints(values []endpoint) []endpoint {
	seen := map[string]bool{}
	var out []endpoint
	for _, value := range values {
		key := fmt.Sprintf("%s|%d|%s", value.Scheme, value.Port, value.CSRFToken)
		if value.Port <= 0 || value.CSRFToken == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

// intPtr returns a pointer to n.
func intPtr(n int) *int { return &n }

// userStatusResponse is the primary Antigravity user-status response.
type userStatusResponse struct {
	Code       *codeValue  `json:"code"`
	Message    string      `json:"message"`
	UserStatus *userStatus `json:"userStatus"`
}

// commandModelConfigResponse is the fallback model-config response.
type commandModelConfigResponse struct {
	Code               *codeValue    `json:"code"`
	Message            string        `json:"message"`
	ClientModelConfigs []modelConfig `json:"clientModelConfigs"`
}

// userStatus contains Antigravity account and model quota data.
type userStatus struct {
	Email                  string          `json:"email"`
	PlanStatus             planStatus      `json:"planStatus"`
	CascadeModelConfigData modelConfigData `json:"cascadeModelConfigData"`
	UserTier               userTier        `json:"userTier"`
}

// PreferredPlanName returns the best displayable Antigravity plan label.
func (s userStatus) PreferredPlanName() string {
	if v := strings.TrimSpace(s.UserTier.Name); v != "" {
		return v
	}
	return s.PlanStatus.PlanInfo.PreferredName()
}

// userTier describes the account tier from Antigravity.
type userTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// planStatus holds Antigravity plan metadata.
type planStatus struct {
	PlanInfo planInfo `json:"planInfo"`
}

// planInfo holds displayable Antigravity plan names.
type planInfo struct {
	PlanName        string `json:"planName"`
	PlanDisplayName string `json:"planDisplayName"`
	DisplayName     string `json:"displayName"`
	ProductName     string `json:"productName"`
	PlanShortName   string `json:"planShortName"`
}

// PreferredName returns the best displayable plan info label.
func (p planInfo) PreferredName() string {
	for _, candidate := range []string{p.PlanDisplayName, p.DisplayName, p.ProductName, p.PlanName, p.PlanShortName} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}

// modelConfigData contains Antigravity client model configs.
type modelConfigData struct {
	ClientModelConfigs []modelConfig `json:"clientModelConfigs"`
}

// modelConfig is one Antigravity model config entry.
type modelConfig struct {
	Label        string     `json:"label"`
	ModelOrAlias modelAlias `json:"modelOrAlias"`
	QuotaInfo    *quotaInfo `json:"quotaInfo"`
}

// modelAlias contains the concrete model identifier.
type modelAlias struct {
	Model string `json:"model"`
}

// quotaInfo contains Antigravity quota fields for one model.
type quotaInfo struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

// codeValue decodes Antigravity response codes that arrive as strings or numbers.
type codeValue struct {
	Raw string
}

// IsOK reports whether the response code represents success.
func (c codeValue) IsOK() bool {
	lower := strings.ToLower(strings.TrimSpace(c.Raw))
	return lower == "" || lower == "0" || lower == "ok" || lower == "success"
}

// UnmarshalJSON decodes a numeric or string Antigravity response code.
func (c *codeValue) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		c.Raw = text
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		c.Raw = number.String()
		return nil
	}
	c.Raw = strings.TrimSpace(string(data))
	return nil
}

// init registers the Antigravity provider with the package registry.
func init() {
	providers.Register(Provider{})
}
