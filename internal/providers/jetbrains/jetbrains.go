// Package jetbrains implements the JetBrains AI local quota provider.
//
// Auth: none. The provider reads JetBrains IDE configuration files and parses
// AIAssistantQuotaManager2.xml.
package jetbrains

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/providerutil"
)

const quotaFileName = "AIAssistantQuotaManager2.xml"

var idePatterns = []struct {
	prefix string
	name   string
}{
	{"IntelliJIdea", "IntelliJ IDEA"},
	{"PyCharm", "PyCharm"},
	{"WebStorm", "WebStorm"},
	{"GoLand", "GoLand"},
	{"CLion", "CLion"},
	{"DataGrip", "DataGrip"},
	{"RubyMine", "RubyMine"},
	{"Rider", "Rider"},
	{"PhpStorm", "PhpStorm"},
	{"AppCode", "AppCode"},
	{"Fleet", "Fleet"},
	{"AndroidStudio", "Android Studio"},
	{"RustRover", "RustRover"},
	{"Aqua", "Aqua"},
	{"DataSpell", "DataSpell"},
}

// ideInfo describes a detected JetBrains IDE configuration directory.
type ideInfo struct {
	Name      string
	Version   string
	BasePath  string
	QuotaPath string
	ModTime   time.Time
}

// quotaInfo is the decoded quotaInfo JSON embedded in JetBrains XML.
type quotaInfo struct {
	Type      string
	Used      float64
	Maximum   float64
	Available float64
	Until     *time.Time
}

// refillInfo is the decoded nextRefill JSON embedded in JetBrains XML.
type refillInfo struct {
	Type     string
	Next     *time.Time
	Amount   *float64
	Duration string
}

// usageSnapshot is the parsed JetBrains AI quota state.
type usageSnapshot struct {
	Quota     quotaInfo
	Refill    *refillInfo
	IDE       *ideInfo
	UpdatedAt time.Time
}

// Provider fetches JetBrains AI quota data.
type Provider struct{}

// ID returns the provider identifier used by the registry.
func (Provider) ID() string { return "jetbrains" }

// Name returns the human-readable provider name.
func (Provider) Name() string { return "JetBrains AI" }

// BrandColor returns the accent color used on button faces.
func (Provider) BrandColor() string { return "#ff3399" }

// BrandBg returns the background color used on button faces.
func (Provider) BrandBg() string { return "#25051a" }

// MetricIDs enumerates the metrics this provider can emit.
func (Provider) MetricIDs() []string {
	return []string{"session-percent"}
}

// Fetch returns the latest JetBrains AI quota snapshot.
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	usage, err := fetchUsage()
	if err != nil {
		return errorSnapshot(err.Error()), nil
	}
	return snapshotFromUsage(usage), nil
}

// fetchUsage locates and parses the JetBrains AI quota file.
func fetchUsage() (usageSnapshot, error) {
	quotaPath, ide, err := resolveQuotaFile()
	if err != nil {
		return usageSnapshot{}, err
	}
	body, err := os.ReadFile(quotaPath)
	if err != nil {
		return usageSnapshot{}, fmt.Errorf("JetBrains AI quota file unreadable: %w", err)
	}
	usage, err := parseXML(body)
	if err != nil {
		return usageSnapshot{}, err
	}
	usage.IDE = ide
	usage.UpdatedAt = time.Now().UTC()
	return usage, nil
}

// resolveQuotaFile returns an override path or the newest detected IDE quota file.
func resolveQuotaFile() (string, *ideInfo, error) {
	if override := firstEnv("CODEXBAR_JETBRAINS_IDE_BASE_PATH", "JETBRAINS_IDE_BASE_PATH", "JETBRAINS_QUOTA_FILE"); override != "" {
		return quotaPathFromOverride(override), nil, nil
	}
	ides := detectInstalledIDEs()
	if len(ides) == 0 {
		return "", nil, errors.New("No JetBrains IDE with AI Assistant quota found. Open a JetBrains IDE with AI Assistant enabled.")
	}
	ide := ides[0]
	return ide.QuotaPath, &ide, nil
}

// firstEnv returns the first non-empty environment variable.
func firstEnv(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// quotaPathFromOverride turns an IDE base path or direct XML path into a quota path.
func quotaPathFromOverride(raw string) string {
	path := expandHome(strings.TrimSpace(raw))
	if strings.EqualFold(filepath.Base(path), quotaFileName) {
		return path
	}
	return filepath.Join(path, "options", quotaFileName)
}

// expandHome expands a leading tilde in a path.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if len(path) == 1 {
				return home
			}
			return filepath.Join(home, strings.TrimLeft(path[2:], `/\`))
		}
	}
	return path
}

// detectInstalledIDEs finds IDE config directories with quota files.
func detectInstalledIDEs() []ideInfo {
	var out []ideInfo
	for _, base := range jetBrainsConfigBasePaths() {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			ide, ok := parseIDEDirectory(entry.Name(), base)
			if !ok {
				continue
			}
			info, err := os.Stat(ide.QuotaPath)
			if err != nil {
				continue
			}
			ide.ModTime = info.ModTime()
			out = append(out, ide)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		if out[i].Name == out[j].Name {
			return compareVersions(out[i].Version, out[j].Version) > 0
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// jetBrainsConfigBasePaths returns platform-specific JetBrains config roots.
func jetBrainsConfigBasePaths() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return nonEmptyPaths(
			joinIfBase(home, "Library", "Application Support", "JetBrains"),
			joinIfBase(home, "Library", "Application Support", "Google"),
		)
	case "windows":
		roaming := os.Getenv("APPDATA")
		local := os.Getenv("LOCALAPPDATA")
		return nonEmptyPaths(
			joinIfBase(roaming, "JetBrains"),
			joinIfBase(roaming, "Google"),
			joinIfBase(local, "JetBrains"),
			joinIfBase(local, "Google"),
			joinIfBase(home, "AppData", "Roaming", "JetBrains"),
			joinIfBase(home, "AppData", "Roaming", "Google"),
		)
	default:
		return nonEmptyPaths(
			joinIfBase(home, ".config", "JetBrains"),
			joinIfBase(home, ".local", "share", "JetBrains"),
			joinIfBase(home, ".config", "Google"),
			joinIfBase(home, ".local", "share", "Google"),
		)
	}
}

// joinIfBase joins path parts when base is available.
func joinIfBase(base string, parts ...string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

// nonEmptyPaths removes paths rooted at an empty base.
func nonEmptyPaths(paths ...string) []string {
	var out []string
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			out = append(out, path)
		}
	}
	return out
}

// parseIDEDirectory maps a config directory name to IDE metadata.
func parseIDEDirectory(dirname string, basePath string) (ideInfo, bool) {
	lower := strings.ToLower(dirname)
	for _, pattern := range idePatterns {
		prefix := strings.ToLower(pattern.prefix)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		version := strings.TrimSpace(dirname[len(pattern.prefix):])
		if version == "" {
			version = "Unknown"
		}
		base := filepath.Join(basePath, dirname)
		return ideInfo{
			Name:      pattern.name,
			Version:   version,
			BasePath:  base,
			QuotaPath: filepath.Join(base, "options", quotaFileName),
		}, true
	}
	return ideInfo{}, false
}

// compareVersions compares dotted numeric version strings.
func compareVersions(a, b string) int {
	ap := versionParts(a)
	bp := versionParts(b)
	maxLen := len(ap)
	if len(bp) > maxLen {
		maxLen = len(bp)
	}
	for i := 0; i < maxLen; i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av != bv {
			return av - bv
		}
	}
	return 0
}

// versionParts returns the numeric parts of a dotted version string.
func versionParts(version string) []int {
	fields := strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		var n int
		for _, r := range field {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

// parseXML extracts embedded JetBrains AI quota JSON from XML.
func parseXML(body []byte) (usageSnapshot, error) {
	options, err := quotaOptions(body)
	if err != nil {
		return usageSnapshot{}, err
	}
	quotaRaw := strings.TrimSpace(options["quotaInfo"])
	if quotaRaw == "" {
		return usageSnapshot{}, errors.New("No quota information found in JetBrains AI configuration.")
	}
	quota, err := parseQuotaInfo(decodeOptionValue(quotaRaw))
	if err != nil {
		return usageSnapshot{}, err
	}
	var refill *refillInfo
	if nextRefill := strings.TrimSpace(options["nextRefill"]); nextRefill != "" {
		if parsed, err := parseRefillInfo(decodeOptionValue(nextRefill)); err == nil {
			refill = &parsed
		}
	}
	return usageSnapshot{Quota: quota, Refill: refill}, nil
}

// quotaOptions returns option values from the AIAssistantQuotaManager2 component.
func quotaOptions(body []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	options := map[string]string{}
	inComponent := false
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("Could not parse JetBrains AI quota XML: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			if inComponent {
				depth++
				if t.Name.Local == "option" {
					name := xmlAttr(t, "name")
					if name == "quotaInfo" || name == "nextRefill" {
						options[name] = xmlAttr(t, "value")
					}
				}
				continue
			}
			if t.Name.Local == "component" && xmlAttr(t, "name") == "AIAssistantQuotaManager2" {
				inComponent = true
				depth = 1
			}
		case xml.EndElement:
			if inComponent {
				depth--
				if depth <= 0 {
					inComponent = false
				}
			}
		}
	}
	return options, nil
}

// xmlAttr returns the value of a start element attribute.
func xmlAttr(el xml.StartElement, name string) string {
	for _, attr := range el.Attr {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

// decodeOptionValue decodes XML and HTML entities from an option value.
func decodeOptionValue(value string) string {
	return html.UnescapeString(strings.ReplaceAll(value, "&#10;", "\n"))
}

// parseQuotaInfo decodes JetBrains quotaInfo JSON.
func parseQuotaInfo(text string) (quotaInfo, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return quotaInfo{}, fmt.Errorf("Could not parse JetBrains AI quota JSON: %w", err)
	}
	used, _ := providerutil.FirstFloat(root, "current", "used")
	maximum, _ := providerutil.FirstFloat(root, "maximum", "max", "limit")
	if maximum <= 0 {
		return quotaInfo{}, errors.New("JetBrains AI quota JSON is missing a maximum.")
	}
	available, ok := providerutil.FirstFloat(root, "available", "remaining")
	if !ok {
		if tariffQuota, ok := providerutil.MapValue(root["tariffQuota"]); ok {
			available, ok = providerutil.FirstFloat(tariffQuota, "available", "remaining")
		}
	}
	if !ok {
		available = math.Max(0, maximum-used)
	}
	var until *time.Time
	if t, ok := providerutil.FirstTime(root, "until"); ok {
		until = t
	}
	return quotaInfo{
		Type:      providerutil.FirstString(root, "type"),
		Used:      math.Max(0, used),
		Maximum:   maximum,
		Available: math.Max(0, math.Min(maximum, available)),
		Until:     until,
	}, nil
}

// parseRefillInfo decodes JetBrains nextRefill JSON.
func parseRefillInfo(text string) (refillInfo, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return refillInfo{}, err
	}
	var next *time.Time
	if t, ok := providerutil.FirstTime(root, "next"); ok {
		next = t
	}
	amount, ok := providerutil.FirstFloat(root, "amount")
	if !ok {
		if tariff, found := providerutil.MapValue(root["tariff"]); found {
			amount, ok = providerutil.FirstFloat(tariff, "amount")
		}
	}
	var amountPtr *float64
	if ok {
		amountPtr = &amount
	}
	duration := providerutil.FirstString(root, "duration")
	if duration == "" {
		if tariff, found := providerutil.MapValue(root["tariff"]); found {
			duration = providerutil.FirstString(tariff, "duration")
		}
	}
	return refillInfo{
		Type:     providerutil.FirstString(root, "type"),
		Next:     next,
		Amount:   amountPtr,
		Duration: duration,
	}, nil
}

// snapshotFromUsage maps JetBrains quota data into Stream Deck metrics.
func snapshotFromUsage(usage usageSnapshot) providers.Snapshot {
	now := usage.UpdatedAt.UTC().Format(time.RFC3339)
	remaining := percentRemaining(usage.Quota)
	ratio := remaining / 100
	value := math.Round(remaining)
	numeric := remaining
	metric := providers.MetricValue{
		ID:           "session-percent",
		Label:        "CURRENT",
		Name:         "JetBrains AI credits remaining",
		Value:        value,
		NumericValue: &numeric,
		NumericUnit:  "percent",
		Unit:         "%",
		Ratio:        &ratio,
		Direction:    "up",
		Caption:      quotaCaption(usage),
		UpdatedAt:    now,
	}
	if usage.Refill != nil && usage.Refill.Next != nil {
		metric.ResetInSeconds = providerutil.ResetSeconds(*usage.Refill.Next)
	}
	if usage.Quota.Maximum > 0 {
		rawCount := int(math.Round(usage.Quota.Available))
		rawMax := int(math.Round(usage.Quota.Maximum))
		metric.RawCount = &rawCount
		metric.RawMax = &rawMax
	}
	return providers.Snapshot{
		ProviderID:   "jetbrains",
		ProviderName: "JetBrains AI",
		Source:       "local",
		Metrics:      []providers.MetricValue{metric},
		Status:       "operational",
	}
}

// percentRemaining returns remaining quota as 0..100.
func percentRemaining(quota quotaInfo) float64 {
	if quota.Maximum <= 0 {
		return 0
	}
	return math.Max(0, math.Min(100, quota.Available/quota.Maximum*100))
}

// quotaCaption builds the compact subvalue shown under the percentage.
func quotaCaption(usage usageSnapshot) string {
	parts := []string{}
	if usage.IDE != nil {
		parts = append(parts, usage.IDE.Name+" "+usage.IDE.Version)
	}
	if usage.Quota.Type != "" {
		parts = append(parts, usage.Quota.Type)
	}
	if usage.Refill != nil && usage.Refill.Duration != "" {
		parts = append(parts, usage.Refill.Duration)
	}
	if len(parts) == 0 {
		return "Quota"
	}
	return strings.Join(parts, " · ")
}

// errorSnapshot returns a JetBrains AI setup or parse failure snapshot.
func errorSnapshot(message string) providers.Snapshot {
	return providers.Snapshot{
		ProviderID:   "jetbrains",
		ProviderName: "JetBrains AI",
		Source:       "local",
		Metrics:      []providers.MetricValue{},
		Status:       "unknown",
		Error:        message,
	}
}

// init registers the JetBrains AI provider with the package registry.
func init() {
	providers.Register(Provider{})
}
