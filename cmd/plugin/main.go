// Usage Buttons — Stream Deck plugin entry point (Go).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/icons"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/claude"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/render"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
	"github.com/anthonybaldwin/UsageButtons/internal/streamdeck"
	"github.com/anthonybaldwin/UsageButtons/internal/update"

	// Register all providers via init().
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/codex"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/copilot"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/cursor"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/kimik2"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/ollama"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/openrouter"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/warp"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/zai"
)

const (
	schedulerTick  = 30 * time.Second
	displayRefresh = 60 * time.Second
	defaultMetric  = "session-percent"
)

// visibleKey tracks a key currently on-screen.
type visibleKey struct {
	context    string
	action     string
	settings   settings.KeySettings
	lastPollAt time.Time
}

var (
	mu          sync.Mutex
	visibleKeys = map[string]*visibleKey{}
)

func main() {
	args := streamdeck.ParseArgs()
	conn, err := streamdeck.Connect(args)
	if err != nil {
		log.Fatalf("[UsageButtons] fatal: %v", err)
	}
	defer conn.Close()

	// Wire logging sinks to Stream Deck's log file.
	providers.LogSink = func(msg string) { conn.Log(msg) }
	update.LogSink = func(msg string) { conn.Log(msg) }
	claude.LogSink = func(msg string) { conn.Log(msg) }

	// Request global settings before first tick.
	conn.GetGlobalSettings()

	// Start scheduler and display refresh tickers.
	go schedulerLoop(conn)
	go displayRefreshLoop(conn)

	// Event loop — blocks forever.
	for {
		ev, err := conn.ReadEvent()
		if err != nil {
			log.Fatalf("[UsageButtons] read error: %v", err)
		}
		handleEvent(conn, ev)
	}
}

func schedulerLoop(conn *streamdeck.Connection) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for range ticker.C {
		scheduleDueKeys(conn)
	}
}

func displayRefreshLoop(conn *streamdeck.Connection) {
	ticker := time.NewTicker(displayRefresh)
	defer ticker.Stop()
	for range ticker.C {
		refreshAllVisible(conn)
	}
}

func scheduleDueKeys(conn *streamdeck.Connection) {
	// Update check — no-ops internally unless the 6h cache expired.
	if !settings.SkipUpdateCheckEnabled() {
		update.Check()
		if update.IsAvailable() {
			showUpdateFace(conn)
			return
		}
	}

	mu.Lock()
	now := time.Now()
	var due []string
	for ctx, key := range visibleKeys {
		interval := time.Duration(settings.ResolveRefreshMs(key.settings)) * time.Millisecond
		if now.Sub(key.lastPollAt) >= interval {
			due = append(due, ctx)
		}
	}
	mu.Unlock()

	for _, ctx := range due {
		refreshKey(conn, ctx, false)
	}
}

func refreshAllVisible(conn *streamdeck.Connection) {
	mu.Lock()
	contexts := make([]string, 0, len(visibleKeys))
	for ctx := range visibleKeys {
		contexts = append(contexts, ctx)
	}
	mu.Unlock()

	for _, ctx := range contexts {
		refreshKey(conn, ctx, false)
	}
}

func handleEvent(conn *streamdeck.Connection, ev streamdeck.Event) {
	switch ev.Event {
	case "willAppear":
		handleWillAppear(conn, ev)
	case "willDisappear":
		handleWillDisappear(conn, ev)
	case "didReceiveSettings":
		handleDidReceiveSettings(conn, ev)
	case "didReceiveGlobalSettings":
		handleDidReceiveGlobalSettings(conn, ev)
	case "sendToPlugin":
		handleSendToPlugin(conn, ev)
	case "keyDown":
		handleKeyDown(conn, ev)
	}
}

func handleWillAppear(conn *streamdeck.Connection, ev streamdeck.Event) {
	providerID := streamdeck.ProviderIDFromAction(ev.Action)
	if providerID == "" {
		return
	}

	var payload streamdeck.WillAppearPayload
	json.Unmarshal(ev.Payload, &payload)

	var ks settings.KeySettings
	json.Unmarshal(payload.Settings, &ks)

	metricID := ks.MetricID
	if metricID == "" {
		metricID = defaultMetric
	}

	// Stale fillColor migration (same as TS version).
	if ks.FillColor != "" {
		prov := providers.Get(providerID)
		stale := []string{"#374151", "#4b5563", "#1e293b", "#ffffff18", "#222e3b", "#3b82f6"}
		if prov != nil {
			stale = append(stale, strings.ToLower(prov.BrandColor()))
		}
		lc := strings.ToLower(ks.FillColor)
		for _, s := range stale {
			if lc == s {
				ks.FillColor = ""
				raw, _ := json.Marshal(ks)
				conn.SetSettings(ev.Context, raw)
				break
			}
		}
	}

	// If an update is pending, show the update face.
	if !settings.SkipUpdateCheckEnabled() && update.IsAvailable() {
		mu.Lock()
		visibleKeys[ev.Context] = &visibleKey{
			context: ev.Context, action: ev.Action,
			settings: ks, lastPollAt: time.Now(),
		}
		mu.Unlock()
		latest := update.LatestVersion()
		if latest == "" {
			latest = "?"
		}
		conn.SetImage(ev.Context, render.RenderButton(render.ButtonInput{
			Label: "UPDATE", Value: "v" + latest,
			Subvalue: "New Version", Fill: "#f59e0b", ValueSize: "medium",
		}))
		return
	}

	// Try to render from cache immediately (avoid loading flash).
	cached := providers.PeekSnapshot(providerID)
	prov := providers.Get(providerID)
	if cached != nil && prov != nil {
		mu.Lock()
		visibleKeys[ev.Context] = &visibleKey{
			context:    ev.Context,
			action:     ev.Action,
			settings:   ks,
			lastPollAt: time.Now(),
		}
		mu.Unlock()

		metric := findMetric(cached.Metrics, metricID)
		if metric != nil {
			conn.SetImage(ev.Context, renderMetric(prov, cached.ProviderName, *metric, ks))
		} else {
			conn.SetImage(ev.Context, placeholderFace(prov, deriveLabelFromMetricID(metricID), "—", "", ks))
		}
		conn.Logf("key appeared with cached data (now tracking %d visible key(s))", countVisible())
		return
	}

	// No cache — first fetch.
	mu.Lock()
	visibleKeys[ev.Context] = &visibleKey{
		context:  ev.Context,
		action:   ev.Action,
		settings: ks,
	}
	mu.Unlock()

	conn.SetImage(ev.Context, loadingFaceFor(providerID, &ks))
	conn.Logf("key appeared, no cache (now tracking %d visible key(s))", countVisible())
	go refreshKey(conn, ev.Context, false)
}

func handleWillDisappear(conn *streamdeck.Connection, ev streamdeck.Event) {
	mu.Lock()
	delete(visibleKeys, ev.Context)
	n := len(visibleKeys)
	mu.Unlock()
	conn.Logf("key disappeared (now tracking %d visible key(s))", n)
}

func handleDidReceiveSettings(conn *streamdeck.Connection, ev streamdeck.Event) {
	var payload streamdeck.DidReceiveSettingsPayload
	json.Unmarshal(ev.Payload, &payload)

	var ks settings.KeySettings
	json.Unmarshal(payload.Settings, &ks)

	mu.Lock()
	key, ok := visibleKeys[ev.Context]
	if ok {
		key.settings = ks
		key.lastPollAt = time.Time{} // reset so scheduler picks it up
	}
	mu.Unlock()

	if ok {
		go refreshKey(conn, ev.Context, false)
	}
}

func handleDidReceiveGlobalSettings(conn *streamdeck.Connection, ev streamdeck.Event) {
	var payload streamdeck.GlobalSettingsPayload
	json.Unmarshal(ev.Payload, &payload)

	var gs settings.GlobalSettings
	json.Unmarshal(payload.Settings, &gs)
	settings.Set(gs)

	conn.Logf("global settings updated")
	go refreshAllVisible(conn)
}

func handleSendToPlugin(conn *streamdeck.Connection, ev streamdeck.Event) {
	var payload streamdeck.SendToPluginPayload
	json.Unmarshal(ev.Payload, &payload)

	if payload.Action == "resetTextSizeOverrides" {
		mu.Lock()
		for ctx, key := range visibleKeys {
			key.settings.ValueSize = ""
			key.settings.SubvalueSize = ""
			raw, _ := json.Marshal(key.settings)
			conn.SetSettings(ctx, raw)
		}
		mu.Unlock()
		go refreshAllVisible(conn)
	}
}

func handleKeyDown(conn *streamdeck.Connection, ev streamdeck.Event) {
	if ev.Context == "" {
		return
	}
	// If an update is available, pressing any button opens the
	// appropriate URL instead of refreshing data.
	if !settings.SkipUpdateCheckEnabled() && update.IsAvailable() {
		conn.OpenURL(update.URL())
		return
	}
	go refreshKey(conn, ev.Context, true)
}

func showUpdateFace(conn *streamdeck.Connection) {
	latest := update.LatestVersion()
	if latest == "" {
		latest = "?"
	}
	svg := render.RenderButton(render.ButtonInput{
		Label:     "UPDATE",
		Value:     "v" + latest,
		Subvalue:  "New Version",
		Fill:      "#f59e0b",
		ValueSize: "medium",
	})
	mu.Lock()
	for ctx := range visibleKeys {
		conn.SetImage(ctx, svg)
	}
	mu.Unlock()
}

func refreshKey(conn *streamdeck.Connection, context string, force bool) {
	mu.Lock()
	key, ok := visibleKeys[context]
	if !ok {
		mu.Unlock()
		return
	}
	key.lastPollAt = time.Now()
	action := key.action
	ks := key.settings
	mu.Unlock()

	providerID := streamdeck.ProviderIDFromAction(action)
	metricID := ks.MetricID
	if metricID == "" {
		metricID = defaultMetric
	}
	if providerID == "" {
		return
	}

	prov := providers.Get(providerID)
	if prov == nil {
		conn.SetImage(context, render.RenderButton(render.ButtonInput{
			Label: "ERR",
			Value: "?",
			Subvalue: providerID,
			Stale: boolPtr(true),
		}))
		return
	}

	snapshot := providers.GetSnapshot(prov, providers.GetSnapshotOptions{Force: force})

	if snapshot.Error != "" && len(snapshot.Metrics) == 0 {
		notConfigured := isNotConfigured(snapshot.Error)
		rateLimit := isRateLimit(snapshot.Error)

		if notConfigured {
			// Park this key.
			mu.Lock()
			if k, ok := visibleKeys[context]; ok {
				k.lastPollAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			mu.Unlock()
		}

		value := "ERR"
		subHint := ""
		if notConfigured {
			value = "SETUP"
			subHint = "needs key"
		} else if rateLimit {
			value = "WAIT"
			subHint = "rate limit"
		}

		conn.SetImage(context, placeholderFace(prov,
			strings.ToUpper(prov.Name()), value, subHint, ks))
		return
	}

	metric := findMetric(snapshot.Metrics, metricID)
	if metric == nil {
		// Synthesize 0% fake for exhausted percent metrics.
		isRequestedPercent := strings.HasSuffix(metricID, "-percent")
		if isRequestedPercent {
			for _, m := range snapshot.Metrics {
				if strings.HasSuffix(m.ID, "-percent") && m.RatioVal() <= 0.01 {
					zero := 0.0
					fake := providers.MetricValue{
						ID:           metricID,
						Label:        deriveLabelFromMetricID(metricID),
						Name:         deriveLabelFromMetricID(metricID) + " (capped)",
						Value:        float64(0),
						NumericValue: &zero,
						NumericUnit:  "percent",
						Unit:         "%",
						Ratio:        &zero,
						Direction:    "up",
					}
					if m.ResetInSeconds != nil {
						fake.ResetInSeconds = m.ResetInSeconds
					}
					conn.SetImage(context, renderMetric(prov, snapshot.ProviderName, fake, ks))
					return
				}
			}
		}

		conn.SetImage(context, placeholderFace(prov,
			deriveLabelFromMetricID(metricID), "—", "", ks))
		return
	}

	conn.SetImage(context, renderMetric(prov, snapshot.ProviderName, *metric, ks))
}

// --- Rendering helpers ---

func renderMetric(prov providers.Provider, providerName string, metric providers.MetricValue, ks settings.KeySettings) string {
	invert := settings.InvertFillEnabled()

	effectiveValue := metric.Value
	effectiveRatio := metric.RatioVal()

	if invert && metric.NumericUnit == "percent" {
		if nv, ok := effectiveValue.(float64); ok {
			effectiveValue = math.Max(0, math.Min(100, 100-nv))
		}
		if effectiveRatio >= 0 {
			effectiveRatio = 1 - effectiveRatio
		}
	}

	valueStr := formatValue(effectiveValue, metric.Unit)

	in := render.ButtonInput{
		Value: valueStr,
	}

	// Label
	if !ks.HideLabel {
		override := strings.TrimSpace(ks.LabelOverride)
		if override != "" {
			in.Label = override
		} else {
			label := metric.Label
			if label == "" {
				label = providerName
			}
			in.Label = strings.ToUpper(label)
		}
	}

	// Ratio
	isReferenceCard := metric.RatioVal() < 0
	if isReferenceCard {
		r := 1.0
		in.Ratio = &r
	} else {
		in.Ratio = &effectiveRatio
	}

	// Direction
	if ks.FillDirection != "" {
		in.Direction = ks.FillDirection
	} else if metric.Direction != "" {
		in.Direction = metric.Direction
	}

	// Fill color: threshold > user override > reference card gray > brand
	thState := computeThresholdState(metric, ks)
	switch thState {
	case "critical":
		c := defStr(ks.CriticalColor, "#ef4444")
		if render.IsValidHexColor(c) {
			in.Fill = c
		}
	case "warn":
		c := defStr(ks.WarnColor, "#f59e0b")
		if render.IsValidHexColor(c) {
			in.Fill = c
		}
	default:
		if ks.FillColor != "" && render.IsValidHexColor(ks.FillColor) {
			in.Fill = ks.FillColor
		} else if isReferenceCard {
			bg := defStr(in.Bg, "#111827")
			in.Fill = render.LightenHex(bg, 0.09)
		} else {
			in.Fill = prov.BrandColor()
		}
	}

	if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
		in.Bg = ks.BgColor
	}
	if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
		in.Fg = ks.TextColor
	}

	// Text sizes
	in.ValueSize = string(resolveTextSize(ks.ValueSize, settings.DefaultValueSz()))
	in.SubvalueSize = string(resolveTextSize(ks.SubvalueSize, settings.DefaultSubvalueSz()))

	if ks.ShowBorder != nil && !*ks.ShowBorder {
		f := false
		in.Border = &f
	}

	// Glyph
	wantGlyph := settings.ShowGlyphsEnabled() && (ks.ShowGlyph == nil || *ks.ShowGlyph)
	if wantGlyph {
		glyph := getProviderGlyph(prov.ID())
		if glyph != nil {
			in.Glyph = glyph
			in.GlyphMode = "watermark"
		}
	} else {
		f := false
		in.ShowGlyph = &f
	}

	// Subvalue priority: countdown > captionOverride > rawCounts > caption > auto
	if metric.ResetInSeconds != nil && (ks.ShowResetTimer == nil || *ks.ShowResetTimer) {
		in.Subvalue = render.FormatCountdown(*metric.ResetInSeconds)
	} else {
		override := strings.TrimSpace(ks.CaptionOverride)
		if override != "" {
			in.Subvalue = override
		} else if resolveShowRawCounts(metric, ks) {
			in.Subvalue = formatRawCounts(metric)
		} else if strings.TrimSpace(metric.Caption) != "" {
			in.Subvalue = metric.Caption
		} else if metric.NumericUnit == "percent" {
			if invert {
				in.Subvalue = "Used"
			} else {
				in.Subvalue = "Remaining"
			}
		}
	}

	if metric.Stale != nil {
		in.Stale = metric.Stale
	}

	return render.RenderButton(in)
}

func placeholderFace(prov providers.Provider, label, value, subvalue string, ks settings.KeySettings) string {
	in := render.ButtonInput{
		Label: label,
		Value: value,
		Fill:  prov.BrandColor(),
	}
	if subvalue != "" {
		in.Subvalue = subvalue
	}
	if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
		in.Bg = ks.BgColor
	}
	if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
		in.Fg = ks.TextColor
	}
	if ks.ShowBorder != nil && !*ks.ShowBorder {
		f := false
		in.Border = &f
	}
	in.ValueSize = string(resolveTextSize(ks.ValueSize, settings.DefaultValueSz()))
	in.SubvalueSize = string(resolveTextSize(ks.SubvalueSize, settings.DefaultSubvalueSz()))

	wantGlyph := settings.ShowGlyphsEnabled() && (ks.ShowGlyph == nil || *ks.ShowGlyph)
	if wantGlyph {
		glyph := getProviderGlyph(prov.ID())
		if glyph != nil {
			in.Glyph = glyph
			in.GlyphMode = "watermark"
		}
	} else {
		f := false
		in.ShowGlyph = &f
	}
	return render.RenderButton(in)
}

func loadingFaceFor(providerID string, ks *settings.KeySettings) string {
	prov := providers.Get(providerID)
	glyph := getProviderGlyph(providerID)
	fillColor := ""
	if prov != nil {
		fillColor = prov.BrandColor()
	}
	var bg, fg string
	var border *bool
	if ks != nil {
		if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
			bg = ks.BgColor
		}
		if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
			fg = ks.TextColor
		}
		if ks.ShowBorder != nil && !*ks.ShowBorder {
			border = ks.ShowBorder
		}
	}
	return render.RenderLoading(glyph, fillColor, bg, fg, border)
}

// --- Threshold logic ---

func computeThresholdState(metric providers.MetricValue, ks settings.KeySettings) string {
	if metric.NumericValue == nil {
		return "normal"
	}
	if metric.NumericUnit != "dollars" && metric.NumericUnit != "cents" {
		return "normal"
	}

	n := *metric.NumericValue
	direction := defStr(metric.NumericGoodWhen, "high")

	var defaultWarn, defaultCritical *float64
	if direction == "high" {
		w, c := 10.0, 0.0
		defaultWarn, defaultCritical = &w, &c
	} else if metric.NumericMax != nil && *metric.NumericMax > 0 {
		w := *metric.NumericMax * 0.8
		c := *metric.NumericMax
		defaultWarn, defaultCritical = &w, &c
	}

	warn := ks.WarnBelow
	if warn == nil {
		warn = defaultWarn
	}
	critical := ks.CriticalBelow
	if critical == nil {
		critical = defaultCritical
	}

	if direction == "high" {
		if critical != nil && n <= *critical {
			return "critical"
		}
		if warn != nil && n <= *warn {
			return "warn"
		}
	} else {
		if critical != nil && n >= *critical {
			return "critical"
		}
		if warn != nil && n >= *warn {
			return "warn"
		}
	}
	return "normal"
}

// --- Utility ---

func resolveShowRawCounts(metric providers.MetricValue, ks settings.KeySettings) bool {
	if ks.ShowRawCounts != nil && !*ks.ShowRawCounts {
		return false
	}
	if metric.RawCount != nil && metric.RawMax != nil {
		return ks.ShowRawCounts == nil || *ks.ShowRawCounts
	}
	if ks.ShowRawCounts != nil && *ks.ShowRawCounts &&
		metric.NumericValue != nil && metric.NumericMax != nil &&
		metric.NumericUnit == "dollars" {
		return true
	}
	return false
}

func formatRawCounts(metric providers.MetricValue) string {
	if metric.RawCount != nil && metric.RawMax != nil {
		return fmt.Sprintf("%d/%d", *metric.RawCount, *metric.RawMax)
	}
	if metric.NumericValue != nil && metric.NumericMax != nil && metric.NumericUnit == "dollars" {
		return fmt.Sprintf("$%.2f/$%.2f", *metric.NumericValue, *metric.NumericMax)
	}
	return ""
}

func formatValue(v any, unit string) string {
	switch val := v.(type) {
	case float64:
		if val == math.Floor(val) {
			return fmt.Sprintf("%d%s", int(val), unit)
		}
		return fmt.Sprintf("%.1f%s", val, unit)
	case int:
		return fmt.Sprintf("%d%s", val, unit)
	case string:
		return val
	default:
		return fmt.Sprintf("%v", v)
	}
}

var knownLabels = map[string]string{
	"session-percent":        "SESSION",
	"session-pace":           "S.PACE",
	"weekly-percent":         "WEEKLY",
	"weekly-pace":            "W.PACE",
	"weekly-sonnet-percent":  "SONNET",
	"weekly-opus-percent":    "OPUS",
	"extra-usage-percent":    "EXTRA USAGE",
	"extra-usage-limit":      "LIMIT",
	"extra-usage-remaining":  "LEFT",
	"extra-usage-spent":      "SPENT",
	"extra-usage-balance":    "BALANCE",
	"extra-usage-enabled":    "EXTRA USAGE",
	"credits-balance":        "CREDITS",
	"credits":                "CREDITS",
	"cost-today":             "TODAY",
	"cost-30d":               "30 DAYS",
}

func deriveLabelFromMetricID(metricID string) string {
	if label, ok := knownLabels[metricID]; ok {
		return label
	}
	parts := strings.SplitN(metricID, "-", 2)
	return strings.ToUpper(parts[0])
}

func findMetric(metrics []providers.MetricValue, id string) *providers.MetricValue {
	for i := range metrics {
		if metrics[i].ID == id {
			return &metrics[i]
		}
	}
	return nil
}

var (
	rateLimitRe     = regexp.MustCompile(`(?i)429|rate.?limit`)
	notConfiguredRe = regexp.MustCompile(`(?i)not.?configured|not found|Set \w+_\w+|Paste a Cookie|No .+ token found`)
)

func isRateLimit(msg string) bool     { return rateLimitRe.MatchString(msg) }
func isNotConfigured(msg string) bool  { return notConfiguredRe.MatchString(msg) }

func countVisible() int {
	return len(visibleKeys)
}

func boolPtr(v bool) *bool { return &v }

func defStr(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

func resolveTextSize(perKey settings.TextSize, global settings.TextSize) settings.TextSize {
	if perKey != "" {
		return perKey
	}
	return global
}

func getProviderGlyph(providerID string) *render.ProviderGlyph {
	return icons.ProviderIcons[providerID]
}
