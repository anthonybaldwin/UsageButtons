// Usage Buttons — Stream Deck plugin entry point (Go).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/icons"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/providers/claude"
	"github.com/anthonybaldwin/UsageButtons/internal/render"
	"github.com/anthonybaldwin/UsageButtons/internal/settings"
	"github.com/anthonybaldwin/UsageButtons/internal/streamdeck"
	"github.com/anthonybaldwin/UsageButtons/internal/update"

	// Register all providers via init().
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/abacus"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/alibaba"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/amp"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/antigravity"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/augment"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/codex"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/copilot"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/cursor"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/factory"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/gemini"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/grok"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/jetbrains"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/kilo"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/kimi"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/kimik2"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/kiro"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/minimax"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/mistral"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/ollama"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/opencode"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/opencodego"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/openrouter"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/perplexity"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/synthetic"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/vertexai"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/warp"
	_ "github.com/anthonybaldwin/UsageButtons/internal/providers/zai"
)

const (
	schedulerTick      = 30 * time.Second
	displayRefresh     = 60 * time.Second
	refreshJitterRatio = 0.20
	defaultMetric      = "session-percent"
	// starfieldFrameTick is the per-frame interval for the animated
	// starfield decoration. Stream Deck rasterizes each SetImage SVG
	// to a static PNG, so animation has to come from re-sending the
	// SVG every frame. 10 Hz keeps drift + shooting-star streaks
	// readable as continuous motion (4 Hz is too choppy for cross-
	// canvas movement, even though it's fine for opacity flicker).
	starfieldFrameTick = 100 * time.Millisecond
)

// visibleKey tracks a key currently on-screen.
type visibleKey struct {
	context       string
	action        string
	settings      settings.KeySettings
	lastPollAt    time.Time
	nextPollAt    time.Time
	showTitle     bool   // true when user has enabled the native SD title
	customTitle   bool   // true when the user has overridden the native title text
	lastAutoTitle string // last title value written by the plugin
}

var (
	mu                 sync.Mutex
	visibleKeys        = map[string]*visibleKey{}
	globalSettingsSeen bool

	autoRegisterOnce sync.Once
)

func globalSettingsLoaded() bool {
	mu.Lock()
	defer mu.Unlock()
	return globalSettingsSeen
}

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
	cookies.LogSink = func(msg string) { conn.Log(msg) }

	// Request global settings before first tick.
	conn.GetGlobalSettings()

	// Auto-register of the native-messaging host is deferred until
	// global settings arrive so we can respect the opt-out flag.

	// Start scheduler and display refresh tickers.
	go schedulerLoop(conn)
	go displayRefreshLoop(conn)
	go starfieldAnimationLoop(conn)

	// Invalidate provider caches when their credential files change,
	// so a post-login tile update arrives within tens of seconds
	// instead of waiting up to MinTTL for the next scheduled poll.
	providers.StartCredentialWatcher()

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
		redrawAllVisible(conn)
	}
}

// starfieldAnimationLoop re-renders visible keys whose provider has the
// starfield decoration enabled (Grok by default) at starfieldFrameTick
// rate. Each redraw resamples time.Now() inside renderStarfield, so the
// star opacities advance one frame and the field shimmers. Skips when
// no global settings yet, and walks only visible keys (so a key that's
// off-screen burns no cycles even if its provider has stars on).
func starfieldAnimationLoop(conn *streamdeck.Connection) {
	ticker := time.NewTicker(starfieldFrameTick)
	defer ticker.Stop()
	for range ticker.C {
		if !globalSettingsLoaded() {
			continue
		}
		mu.Lock()
		var animateCtxs []string
		for ctx, key := range visibleKeys {
			providerID := streamdeck.ProviderIDFromAction(key.action)
			if providerID == "" {
				continue
			}
			if settings.StarfieldEnabled(providerID, key.settings) {
				animateCtxs = append(animateCtxs, ctx)
			}
		}
		mu.Unlock()
		for _, ctx := range animateCtxs {
			redrawKeyFromCache(conn, ctx)
		}
	}
}

func scheduleDueKeys(conn *streamdeck.Connection) {
	if !globalSettingsLoaded() {
		return
	}

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
		providerID := streamdeck.ProviderIDFromAction(key.action)
		interval := time.Duration(settings.ResolveRefreshMs(key.settings, providerID)) * time.Millisecond
		if key.nextPollAt.IsZero() && !key.lastPollAt.IsZero() {
			key.nextPollAt = nextPollTime(key.lastPollAt, interval, ctx, providerID)
		}
		if key.lastPollAt.IsZero() || (!key.nextPollAt.IsZero() && !now.Before(key.nextPollAt)) {
			due = append(due, ctx)
		}
	}
	mu.Unlock()

	for _, ctx := range due {
		go refreshKey(conn, ctx, false)
	}
}

func nextPollTime(from time.Time, interval time.Duration, context, providerID string) time.Time {
	if from.IsZero() || interval <= 0 {
		return from
	}
	return from.Add(interval + refreshJitter(interval, providerID+"|"+context))
}

func refreshJitter(interval time.Duration, seed string) time.Duration {
	max := time.Duration(float64(interval) * refreshJitterRatio)
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return time.Duration(h.Sum64() % uint64(max+1))
}

func refreshAllVisible(conn *streamdeck.Connection) {
	mu.Lock()
	contexts := make([]string, 0, len(visibleKeys))
	for ctx := range visibleKeys {
		contexts = append(contexts, ctx)
	}
	mu.Unlock()

	for _, ctx := range contexts {
		go refreshKey(conn, ctx, false)
	}
}

func refreshOrRedrawVisible(conn *streamdeck.Connection) {
	type visibleContext struct {
		context    string
		providerID string
	}
	mu.Lock()
	contexts := make([]visibleContext, 0, len(visibleKeys))
	for ctx, key := range visibleKeys {
		contexts = append(contexts, visibleContext{
			context:    ctx,
			providerID: streamdeck.ProviderIDFromAction(key.action),
		})
	}
	mu.Unlock()

	for _, item := range contexts {
		if snapshot, _ := providers.PeekSnapshotState(item.providerID); snapshot != nil {
			stampCacheRedrawPollTime(item.context, item.providerID)
			redrawKeyFromCache(conn, item.context)
			continue
		}
		refreshKey(conn, item.context, false)
	}
}

func stampCacheRedrawPollTime(context, providerID string) {
	mu.Lock()
	defer mu.Unlock()
	key, ok := visibleKeys[context]
	if !ok {
		return
	}
	now := time.Now()
	interval := time.Duration(settings.ResolveRefreshMs(key.settings, providerID)) * time.Millisecond
	key.lastPollAt = now
	key.nextPollAt = nextPollTime(now, interval, context, providerID)
}

func redrawAllVisible(conn *streamdeck.Connection) {
	if !globalSettingsLoaded() {
		return
	}

	mu.Lock()
	contexts := make([]string, 0, len(visibleKeys))
	for ctx := range visibleKeys {
		contexts = append(contexts, ctx)
	}
	mu.Unlock()

	for _, ctx := range contexts {
		redrawKeyFromCache(conn, ctx)
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
	case "titleParametersDidChange":
		handleTitleParametersDidChange(conn, ev)
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

	metricID := effectiveMetricID(ks, providerID)

	// Check if the user has "Show Title" enabled for this key.
	var tp streamdeck.WillAppearTitleParameters
	json.Unmarshal(ev.Payload, &tp)
	showTitle := tp.TitleParameters != nil && tp.TitleParameters.ShowTitle
	title := deriveLabelFromMetricID(metricID)
	customTitle := isCustomTitle(payload.Title, title)
	lastAutoTitle := title

	// Stale fillColor / bgColor migration.
	{
		prov := providers.Get(providerID)
		dirty := false

		// Clear stale fill colors (old brand colors + legacy defaults).
		if ks.FillColor != "" {
			staleFill := []string{
				"#374151", "#4b5563", "#1e293b", "#ffffff18", "#222e3b", "#3b82f6",
				// Old brand colors (pre-v0.3).
				"#49a3b0", "#a855f7", "#00bfa5", "#888888", "#938bb4", "#e85a6a", "#4c00ff",
				// Old Codex green (pre blue/purple gradient).
				"#10a37f",
				// Old provider-parity colors (pre-CodexBar palette sync).
				"#8534f3", "#f54e00", "#01a4ff", "#0071e3",
			}
			if prov != nil {
				staleFill = append(staleFill, strings.ToLower(prov.BrandColor()))
				if prov.ID() == "zai" {
					staleFill = append(staleFill, "#ffffff")
				}
			}
			lc := strings.ToLower(ks.FillColor)
			for _, s := range staleFill {
				if lc == s {
					ks.FillColor = ""
					dirty = true
					break
				}
			}
		}

		// Clear stale bg colors (old default).
		if ks.BgColor != "" {
			staleBg := []string{"#111827"}
			lc := strings.ToLower(ks.BgColor)
			for _, s := range staleBg {
				if lc == s {
					ks.BgColor = ""
					dirty = true
					break
				}
			}
		}

		if dirty {
			raw, _ := json.Marshal(ks)
			conn.SetSettings(ev.Context, raw)
		}
	}

	// If an update is pending, show the update face.
	if !settings.SkipUpdateCheckEnabled() && update.IsAvailable() {
		now := time.Now()
		mu.Lock()
		visibleKeys[ev.Context] = &visibleKey{
			context: ev.Context, action: ev.Action,
			settings: ks, lastPollAt: now,
			showTitle: showTitle,
		}
		mu.Unlock()
		latest := update.LatestVersion()
		if latest == "" {
			latest = "?"
		}
		conn.SetImage(ev.Context, render.RenderButton(render.ButtonInput{
			Label:    "UPDATE",
			Value:    "v" + latest,
			Subvalue: "New Version", Fill: "#f59e0b", ValueSize: "medium",
		}))
		conn.SetTitle(ev.Context, "")
		return
	}

	hideLabel := showTitle

	// Try to render from cache immediately (avoid loading flash). Route
	// through renderSnapshotForKey so cached error faces, stale markers,
	// and reset-timer aging behave identically to the minute redraw.
	prov := providers.Get(providerID)
	var snapshot *providers.Snapshot
	var fetchedAt time.Time
	settingsLoaded := globalSettingsLoaded()
	if settingsLoaded {
		snapshot, fetchedAt = providers.PeekSnapshotState(providerID)
	}
	if snapshot != nil && prov != nil {
		if metric := findMetric(snapshot.Metrics, metricID); metric != nil {
			metricLabel := strings.ToUpper(metric.Label)
			if metricLabel == "" {
				metricLabel = title
			}
			customTitle = isCustomTitle(payload.Title, title, metricLabel)
			lastAutoTitle = metricLabel
		}
		mu.Lock()
		now := time.Now()
		interval := time.Duration(settings.ResolveRefreshMs(ks, providerID)) * time.Millisecond
		visibleKeys[ev.Context] = &visibleKey{
			context:       ev.Context,
			action:        ev.Action,
			settings:      ks,
			lastPollAt:    now,
			nextPollAt:    nextPollTime(now, interval, ev.Context, providerID),
			showTitle:     showTitle,
			customTitle:   customTitle,
			lastAutoTitle: lastAutoTitle,
		}
		keyCopy := *visibleKeys[ev.Context]
		mu.Unlock()

		age := time.Duration(0)
		if !fetchedAt.IsZero() {
			age = time.Since(fetchedAt)
		}
		renderSnapshotForKey(conn, keyCopy, prov, *snapshot, age)
		conn.Logf("key appeared with cached data (now tracking %d visible key(s))", countVisible())
		return
	}

	// No cache — first fetch.
	mu.Lock()
	visibleKeys[ev.Context] = &visibleKey{
		context:       ev.Context,
		action:        ev.Action,
		settings:      ks,
		showTitle:     showTitle,
		customTitle:   customTitle,
		lastAutoTitle: lastAutoTitle,
	}
	mu.Unlock()

	if prov := providers.Get(providerID); prov != nil {
		svgLabel := ""
		if !hideLabel {
			svgLabel = title
		}
		// Merge provider-tier overrides so the initial placeholder
		// reflects the same colors the live tile will render with.
		ksEff := settings.EffectiveSettings(ks, prov.ID())
		conn.SetImage(ev.Context, placeholderFace(prov, svgLabel, "", "", ksEff))
	} else {
		ksEff := settings.EffectiveSettings(ks, providerID)
		conn.SetImage(ev.Context, loadingFaceFor(providerID, &ksEff))
	}
	setTitleForKey(conn, ev.Context, customTitle, title)
	conn.Logf("key appeared, no cache (now tracking %d visible key(s))", countVisible())
	if settingsLoaded {
		go refreshKey(conn, ev.Context, false)
	} else {
		conn.Logf("key appeared before global settings; waiting to refresh")
	}
}

func handleWillDisappear(conn *streamdeck.Connection, ev streamdeck.Event) {
	mu.Lock()
	delete(visibleKeys, ev.Context)
	n := len(visibleKeys)
	mu.Unlock()
	conn.Logf("key disappeared (now tracking %d visible key(s))", n)
}

func handleTitleParametersDidChange(conn *streamdeck.Connection, ev streamdeck.Event) {
	var payload streamdeck.TitleParametersDidChangePayload
	json.Unmarshal(ev.Payload, &payload)

	mu.Lock()
	key, ok := visibleKeys[ev.Context]
	if ok {
		key.showTitle = payload.TitleParameters.ShowTitle
		providerID := streamdeck.ProviderIDFromAction(key.action)
		key.customTitle = isCustomTitle(
			payload.Title,
			key.lastAutoTitle,
			deriveLabelFromMetricID(effectiveMetricID(key.settings, providerID)),
		)
	}
	mu.Unlock()

	if ok {
		go refreshKey(conn, ev.Context, false)
	}
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
		key.nextPollAt = time.Time{}
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

	mu.Lock()
	firstGlobalSettings := !globalSettingsSeen
	globalSettingsSeen = true
	mu.Unlock()

	// Migrate away from legacy sentinel plugin-tier colors. Earlier
	// builds persisted the color-picker's default value on every save
	// regardless of user intent (fixed in cccbd31), so existing
	// installs may still carry the old sentinels in GlobalSettings.
	// Strip them here — unset = fall through to brand bg / brand
	// color as the natural provider-level default.
	legacyDirty := false
	if strings.EqualFold(gs.DefaultBgColor, "#111827") {
		gs.DefaultBgColor = ""
		legacyDirty = true
	}
	if strings.EqualFold(gs.DefaultFillColor, "#3b82f6") {
		gs.DefaultFillColor = ""
		legacyDirty = true
	}

	// Diff the previous snapshot against the incoming one before we
	// overwrite it, so we can drop cache entries for providers whose
	// credentials or endpoint overrides just changed. Without this the
	// next poll would serve the old snapshot for up to MinTTL.
	prevKeys := settings.ProviderKeysGet()
	settings.Set(gs)
	if legacyDirty {
		raw, _ := json.Marshal(gs)
		conn.SetGlobalSettings(raw)
	}
	for _, id := range settings.ChangedProviderIDs(prevKeys, gs.ProviderKeys) {
		if firstGlobalSettings {
			providers.ClearRuntimeCache(id)
			conn.Logf("provider config loaded — cleared runtime cache for %s", id)
		} else {
			providers.ClearCache(id)
			conn.Logf("provider config changed — cleared cache for %s", id)
		}
	}

	conn.Logf("global settings updated")
	if firstGlobalSettings {
		go refreshOrRedrawVisible(conn)
	} else {
		go refreshAllVisible(conn)
	}

	// Auto-register the native-messaging host on the first global
	// settings event (i.e. plugin startup), unless the user has
	// explicitly opted out by clicking Unregister.
	autoRegisterOnce.Do(func() {
		if gs.CookieHostOptedOut {
			conn.Logf("cookie host auto-register skipped (user opted out)")
			return
		}
		go func() {
			if err := registerCookieHost(); err != nil {
				conn.Logf("cookie host auto-register: %v", err)
			} else {
				conn.Logf("cookie host auto-register ok (%s)", cookies.DefaultExtensionID)
			}
		}()
	})
}

func handleSendToPlugin(conn *streamdeck.Connection, ev streamdeck.Event) {
	var payload streamdeck.SendToPluginPayload
	json.Unmarshal(ev.Payload, &payload)

	switch payload.Action {
	case "resetTextSizeOverrides":
		mu.Lock()
		for ctx, key := range visibleKeys {
			key.settings.ValueSize = ""
			key.settings.SubvalueSize = ""
			raw, _ := json.Marshal(key.settings)
			conn.SetSettings(ctx, raw)
		}
		mu.Unlock()
		go refreshAllVisible(conn)
	case "resetPluginStyleDefaults":
		// Cascade reset: clear every style knob at all three tiers so
		// the deck snaps back to brand / built-in defaults everywhere.
		// Non-style state is preserved:
		//   - Plugin: refresh interval, skip-update-check, extension
		//     opt-out, providerKeys (credentials / endpoints)
		//   - Button: metricId (tile identity)
		gs := settings.Get()
		gs.DefaultValueSize = ""
		gs.DefaultSubvalueSize = ""
		gs.DefaultTextColor = ""
		gs.DefaultFillColor = ""
		gs.DefaultBgColor = ""
		gs.DefaultShowBorder = nil
		gs.DefaultFillDirection = ""
		gs.DefaultShowResetTimer = nil
		gs.DefaultShowRawCounts = nil
		gs.DefaultHideSubvalue = nil
		gs.DefaultWarnBelow = nil
		gs.DefaultWarnColor = ""
		gs.DefaultCriticalBelow = nil
		gs.DefaultCriticalColor = ""
		gs.InvertFill = false
		gs.ShowGlyphs = nil
		// Provider-tier overrides are all style-only (ProviderSettings
		// holds no credentials — those live in ProviderKeys). Safe to
		// drop the whole map.
		gs.ProviderSettings = nil
		settings.Set(gs)
		raw, _ := json.Marshal(gs)
		conn.SetGlobalSettings(raw)

		// Button tier: walk every visible key, zero its style fields,
		// persist, and redraw. MetricID stays so tiles keep showing
		// what they're supposed to show; only the appearance resets.
		mu.Lock()
		for ctx, key := range visibleKeys {
			ks := &key.settings
			ks.RefreshMinutes = nil
			ks.WarnBelow = nil
			ks.CriticalBelow = nil
			ks.WarnColor = ""
			ks.CriticalColor = ""
			ks.LabelOverride = ""
			ks.HideLabel = false
			ks.CaptionOverride = ""
			ks.FillColor = ""
			ks.BgColor = ""
			ks.TextColor = ""
			ks.FillDirection = ""
			ks.ValueSize = ""
			ks.SubvalueSize = ""
			ks.ShowBorder = nil
			ks.ShowGlyph = nil
			ks.ShowResetTimer = nil
			ks.ShowRawCounts = nil
			ks.HideSubvalue = nil
			rawKS, _ := json.Marshal(*ks)
			conn.SetSettings(ctx, rawKS)
		}
		mu.Unlock()

		// Style-only change — no need to re-poll providers. Redraw
		// from cached snapshots with the fresh defaults applied.
		go redrawAllVisible(conn)
	case "getCookieStatus":
		go replyCookieStatus(conn, ev.Context, ev.Action)
	case "registerCookieHost":
		go replyRegisterCookieHost(conn, ev.Context, ev.Action, ev.Payload)
	case "unregisterCookieHost":
		go replyUnregisterCookieHost(conn, ev.Context, ev.Action)
	case "getProviderStatus":
		go replyProviderStatus(conn, ev.Context, ev.Action)
	}
}

// cookieHostPayload is the PI → plugin shape for registerCookieHost.
func replyCookieStatus(conn *streamdeck.Connection, ctxStr, action string) {
	pctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	status, reachable, probeErr := cookies.StatusDetail(pctx)
	latest := update.LatestVersion()
	helperAllowlistStale := status.Ready && !cookies.HelperAllowlistCurrent(status.AllowedHosts)
	helperAvailable := status.Ready
	updateAvailable := status.Version != "" && latest != "" && update.IsNewerVersion(status.Version, latest)

	// Diagnostic line for the "Not connected" failure mode. `reachable`
	// separates "plugin couldn't dial the native-host socket" (plugin
	// or host side) from "native host answered but extension isn't
	// attached" (extension side). Both show ready=false in the PI but
	// have very different root causes.
	probeErrStr := ""
	if probeErr != nil {
		probeErrStr = probeErr.Error()
	}
	conn.Logf("cookieStatus poll: ready=%v allowlistCurrent=%v ext-version=%q reachable=%v probeErr=%q registered=%v",
		status.Ready, !helperAllowlistStale, status.Version, reachable, probeErrStr,
		cookies.IsHostRegistered(cookies.HostName))

	conn.SendToPropertyInspector(ctxStr, action, map[string]any{
		"action":               "cookieStatus",
		"available":            helperAvailable,
		"extensionVersion":     status.Version,
		"extensionAllowlistOK": !helperAllowlistStale,
		"latestVersion":        latest,
		"updateAvailable":      updateAvailable,
		"ipcAddress":           cookies.IPCAddress(),
		"hostName":             cookies.HostName,
		"hostRegistered":       cookies.IsHostRegistered(cookies.HostName),
	})
}

func replyRegisterCookieHost(conn *streamdeck.Connection, ctxStr, action string, raw json.RawMessage) {
	result := map[string]any{"action": "cookieHostRegistered"}
	if err := registerCookieHost(); err != nil {
		result["success"] = false
		result["error"] = err.Error()
		conn.SendToPropertyInspector(ctxStr, action, result)
		return
	}
	result["success"] = true
	result["hostName"] = cookies.HostName
	conn.SendToPropertyInspector(ctxStr, action, result)
	setCookieHostOptedOut(conn, false)
}

// registerCookieHost writes the native-messaging manifest (+ registry
// keys on Windows) for every supported browser using the deterministic
// DefaultExtensionID. Idempotent — safe to call on every plugin
// launch.
func registerCookieHost() error {
	hostPath, err := nativeHostBinaryPath()
	if err != nil {
		return fmt.Errorf("locate native-host binary: %w", err)
	}
	if _, err := os.Stat(hostPath); err != nil {
		return fmt.Errorf("native-host binary missing at %s — rebuild the plugin", hostPath)
	}
	origins := []string{cookies.ExtensionOrigin(cookies.DefaultExtensionID)}
	return cookies.RegisterHost(cookies.HostName, hostPath, origins)
}

func replyUnregisterCookieHost(conn *streamdeck.Connection, ctxStr, action string) {
	result := map[string]any{"action": "cookieHostUnregistered"}
	if err := cookies.UnregisterHost(cookies.HostName); err != nil {
		result["success"] = false
		result["error"] = err.Error()
	} else {
		result["success"] = true
	}
	conn.SendToPropertyInspector(ctxStr, action, result)
	setCookieHostOptedOut(conn, true)
}

func replyProviderStatus(conn *streamdeck.Connection, ctxStr, action string) {
	providerID := streamdeck.ProviderIDFromAction(action)
	prov := providers.Get(providerID)
	if prov == nil {
		return
	}
	var snapshot *providers.Snapshot
	if globalSettingsLoaded() {
		snapshot, _ = providers.PeekSnapshotState(providerID)
	}
	errText := ""
	if snapshot != nil {
		errText = snapshot.Error
	}
	conn.SendToPropertyInspector(ctxStr, action, map[string]any{
		"action":     "providerStatus",
		"providerId": providerID,
		"error":      errText,
	})
}

// setCookieHostOptedOut persists the opt-out flag in global settings
// so auto-register on next launch respects the user's choice.
func setCookieHostOptedOut(conn *streamdeck.Connection, optedOut bool) {
	gs := settings.Get()
	gs.CookieHostOptedOut = optedOut
	settings.Set(gs)
	raw, _ := json.Marshal(gs)
	conn.SetGlobalSettings(raw)
}

// nativeHostBinaryPath resolves the companion native-host binary that
// ships alongside the main plugin binary.
func nativeHostBinaryPath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exePath)
	var name string
	switch runtime.GOOS {
	case "windows":
		name = "usagebuttons-native-host-win.exe"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			name = "usagebuttons-native-host-mac-arm64"
		} else {
			name = "usagebuttons-native-host-mac-x64"
		}
	default:
		return "", errors.New("native host is only supported on Windows and macOS")
	}
	return filepath.Join(dir, name), nil
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
	providerID := streamdeck.ProviderIDFromAction(ev.Action)
	skipContext := ev.Context
	go func() {
		refreshKey(conn, skipContext, true)
		// The forced fetch just populated the provider cache. Re-render
		// sibling keys of the same provider so a single press updates
		// every button that shares the data source (e.g. pressing the
		// SESSION key also refreshes WEEKLY, which comes from the same
		// OAuth response). Siblings pass force=false so they hit the
		// warm cache — no extra upstream calls.
		if providerID != "" {
			refreshProviderSiblings(conn, providerID, skipContext)
		}
	}()
}

// refreshProviderSiblings re-renders every visible key of providerID
// except skipContext. Intended to run after a forced fetch so all
// buttons fed by one snapshot update together.
func refreshProviderSiblings(conn *streamdeck.Connection, providerID, skipContext string) {
	mu.Lock()
	var siblings []string
	for ctx, key := range visibleKeys {
		if ctx == skipContext {
			continue
		}
		if streamdeck.ProviderIDFromAction(key.action) == providerID {
			siblings = append(siblings, ctx)
		}
	}
	mu.Unlock()

	for _, ctx := range siblings {
		refreshKey(conn, ctx, false)
	}
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
		conn.SetTitle(ctx, "")
	}
	mu.Unlock()
}

func refreshKey(conn *streamdeck.Connection, context string, force bool) {
	if !globalSettingsLoaded() {
		return
	}

	mu.Lock()
	key, ok := visibleKeys[context]
	if !ok {
		mu.Unlock()
		return
	}
	now := time.Now()
	action := key.action
	ks := key.settings
	providerID := streamdeck.ProviderIDFromAction(action)
	interval := time.Duration(settings.ResolveRefreshMs(ks, providerID)) * time.Millisecond
	key.lastPollAt = now
	key.nextPollAt = nextPollTime(now, interval, context, providerID)
	showTitle := key.showTitle
	customTitle := key.customTitle
	mu.Unlock()

	if providerID == "" {
		return
	}

	prov := providers.Get(providerID)
	if prov == nil {
		conn.SetImage(context, render.RenderButton(render.ButtonInput{
			Value:    "?",
			Subvalue: providerID,
			Stale:    boolPtr(true),
		}))
		conn.SetTitle(context, "ERR")
		return
	}

	snapshot := providers.GetSnapshot(prov, providers.GetSnapshotOptions{Force: force})
	renderSnapshotForKey(conn, visibleKey{
		context:     context,
		action:      action,
		settings:    ks,
		showTitle:   showTitle,
		customTitle: customTitle,
	}, prov, snapshot, 0)
}

func redrawKeyFromCache(conn *streamdeck.Connection, context string) {
	if !globalSettingsLoaded() {
		return
	}

	mu.Lock()
	key, ok := visibleKeys[context]
	if !ok {
		mu.Unlock()
		return
	}
	keyCopy := *key
	mu.Unlock()

	providerID := streamdeck.ProviderIDFromAction(keyCopy.action)
	if providerID == "" {
		return
	}
	prov := providers.Get(providerID)
	if prov == nil {
		return
	}
	snapshot, fetchedAt := providers.PeekSnapshotState(providerID)
	if snapshot == nil {
		return
	}
	age := time.Duration(0)
	if !fetchedAt.IsZero() {
		age = time.Since(fetchedAt)
	}
	renderSnapshotForKey(conn, keyCopy, prov, *snapshot, age)
}

func renderSnapshotForKey(conn *streamdeck.Connection, key visibleKey, prov providers.Provider, snapshot providers.Snapshot, snapshotAge time.Duration) {
	context := key.context
	// Merge provider-tier overrides under the per-button settings so
	// every downstream consumer sees a single resolved KeySettings.
	// Button values win; provider overrides fill in the rest.
	ks := settings.EffectiveSettings(key.settings, prov.ID())
	hideLabel := key.showTitle
	customTitle := key.customTitle
	metricID := effectiveMetricID(ks, prov.ID())
	title := deriveLabelFromMetricID(metricID)

	if snapshot.Error != "" && len(snapshot.Metrics) == 0 {
		notConfigured := isNotConfigured(snapshot.Error)
		rateLimit := isRateLimit(snapshot.Error)

		if notConfigured {
			// Park this key.
			mu.Lock()
			if k, ok := visibleKeys[context]; ok {
				k.lastPollAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
				k.nextPollAt = k.lastPollAt
			}
			mu.Unlock()
		}

		value := "ERR"
		subHint := ""
		switch {
		case notConfigured:
			value = "SETUP"
			if isExtensionNeeded(snapshot.Error) {
				subHint = "Needs ext."
			} else {
				subHint = "Needs Key"
			}
		case rateLimit:
			value = "WAIT"
			subHint = "Rate Limit"
		case isExpired(snapshot.Error):
			value = "AUTH"
			subHint = "Re-auth Needed"
		case isSignedOut(snapshot.Error):
			value = "AUTH"
			subHint = "Signed Out"
		case isMissingScope(snapshot.Error):
			value = "AUTH"
			subHint = "Bad Scope"
		case isExtensionTimeout(snapshot.Error):
			value = "EXT"
			subHint = "Slow Response"
		case isNetworkError(snapshot.Error):
			value = "NET"
			subHint = "Offline"
		case isServerError(snapshot.Error):
			value = "ERR"
			subHint = "Server Error"
		default:
			subHint = "See Settings"
		}

		svgLabel := ""
		if !hideLabel {
			svgLabel = title
		}
		conn.Logf("render[%s/%s] error-face: value=%q sub=%q err=%q",
			prov.ID(), metricID, value, subHint, snapshot.Error)
		conn.SetImage(context, placeholderFace(prov,
			svgLabel, value, subHint, ks))
		setTitleForKey(conn, context, customTitle, title)
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
					fake = metricWithSnapshotAge(fake, snapshotAge)
					conn.SetImage(context, renderMetric(prov, snapshot.ProviderName, fake, ks, hideLabel))
					setTitleForKey(conn, context, customTitle, title)
					return
				}
			}
		}

		// Dashed-out placeholder — show metric caption as subtext
		// so the user knows what this button is for.
		caption := metricCaptionForPlaceholder(metricID)
		svgLabel := ""
		if !hideLabel {
			svgLabel = title
		}
		conn.Logf("render[%s/%s] no-metric placeholder: caption=%q (snapshot had %d metrics)",
			prov.ID(), metricID, caption, len(snapshot.Metrics))
		conn.SetImage(context, placeholderFace(prov,
			svgLabel, "—", caption, ks))
		setTitleForKey(conn, context, customTitle, title)
		return
	}

	metricCopy := metricWithSnapshotAge(*metric, snapshotAge)
	// Use the metric's own label for the title (uppercased).
	metricLabel := strings.ToUpper(metricCopy.Label)
	if metricLabel == "" {
		metricLabel = title
	}
	conn.Logf("render[%s/%s] metric: value=%v unit=%s ratio=%v source=%s",
		prov.ID(), metricID, metricCopy.Value, metricCopy.Unit, metricCopy.Ratio, snapshot.Source)
	conn.SetImage(context, renderMetric(prov, snapshot.ProviderName, metricCopy, ks, hideLabel))
	setTitleForKey(conn, context, customTitle, metricLabel)
}

// setTitleForKey pre-populates the native title with our label so
// that when the user enables "Show Title", they see a 1:1 match.
// When ShowTitle is off (default), the SD hides it anyway.
// When ShowTitle is on, we still set it so the field stays current
// if the user hasn't edited the text.
func setTitleForKey(conn *streamdeck.Connection, context string, preserveUserTitle bool, label string) {
	if preserveUserTitle {
		return
	}
	mu.Lock()
	if key, ok := visibleKeys[context]; ok {
		key.lastAutoTitle = label
	}
	mu.Unlock()
	conn.SetTitle(context, label)
}

// --- Rendering helpers ---

func renderMetric(prov providers.Provider, providerName string, metric providers.MetricValue, ks settings.KeySettings, hideLabel bool) string {
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
		Value:         valueStr,
		SmartContrast: settings.SmartContrastFor(prov.ID()),
		Starfield:     settings.StarfieldEnabled(prov.ID(), ks),
	}

	// Label: render in SVG unless the user has set a custom native
	// title (in which case we let the SDK title show instead).
	if !hideLabel {
		label := metric.Label
		if label == "" {
			label = providerName
		}
		in.Label = strings.ToUpper(label)
	}

	// Ratio: meter metrics get the effective ratio; reference cards
	// (metric.Ratio == nil) leave in.Ratio nil so fillRect draws
	// nothing. No more LightenHex "card" treatment — the user wants
	// one consistent bg everywhere; fill is the only thing that
	// differs between meter and reference tiles.
	if metric.Ratio != nil {
		in.Ratio = &effectiveRatio
	}

	// Direction: button override > provider override > plugin default >
	// metric-provided direction. ks is already merged with provider
	// overrides via EffectiveSettings, so ks.FillDirection being set
	// means button or provider tier chose it.
	if ks.FillDirection != "" {
		in.Direction = ks.FillDirection
	} else if d := settings.DefaultFillDirectionValue(); d != "" && d != "up" {
		// Only apply the plugin default when the user has explicitly
		// changed it from "up"; otherwise let metric.Direction (which
		// encodes "up for remaining, down for used") take the lead.
		in.Direction = d
	} else if metric.Direction != "" {
		in.Direction = metric.Direction
	}

	// Background: button/provider override > plugin default > brand bg.
	// Brand is the natural provider-level default — Claude brown, Codex
	// black, etc. — so tiles feel at home on their provider. Reference
	// cards simply don't fill on top (in.Ratio stays nil), so the brand
	// bg shows through as a solid tile without the old LightenHex trick.
	in.Bg = prov.BrandBg()
	if v := settings.DefaultBgColorValue(); v != "" && render.IsValidHexColor(v) {
		in.Bg = v
	}
	if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
		in.Bg = ks.BgColor
	}
	// Text color: button/provider override > plugin default > hardcoded.
	// The renderer handles contrast with bg/fill itself via a dual-
	// layer draw, so we don't need a "brand text" fallback here.
	if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
		in.Fg = ks.TextColor
	} else if v := settings.DefaultTextColorValue(); render.IsValidHexColor(v) {
		in.Fg = v
	}

	// Fill color: threshold > button/provider override > plugin default
	// > brand. Reference cards don't actually draw a fill (in.Ratio is
	// nil above) so their computed in.Fill is cosmetic; we still pick a
	// sensible value so the threshold-state fallback and the brand
	// color path both work for meter metrics.
	thState := computeThresholdState(metric, ks)
	switch thState {
	case "critical":
		c := defStr(ks.CriticalColor, settings.DefaultCriticalColorValue())
		if render.IsValidHexColor(c) {
			in.Fill = c
		}
	case "warn":
		c := defStr(ks.WarnColor, settings.DefaultWarnColorValue())
		if render.IsValidHexColor(c) {
			in.Fill = c
		}
	default:
		if ks.FillColor != "" && render.IsValidHexColor(ks.FillColor) {
			in.Fill = ks.FillColor
		} else if v := settings.DefaultFillColorValue(); v != "" && render.IsValidHexColor(v) {
			in.Fill = v
		} else {
			in.Fill = prov.BrandColor()
		}
	}

	// Text sizes
	in.ValueSize = string(resolveTextSize(ks.ValueSize, settings.DefaultValueSz()))
	in.SubvalueSize = string(resolveTextSize(ks.SubvalueSize, settings.DefaultSubvalueSz()))

	// Border: button override > provider override > plugin default.
	// Button/provider values are merged into ks by EffectiveSettings;
	// when still unset, fall back to the plugin default.
	borderOn := settings.DefaultShowBorderEnabled()
	if ks.ShowBorder != nil {
		borderOn = *ks.ShowBorder
	}
	if !borderOn {
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

	// Subvalue priority: hideSubvalue > countdown > captionOverride > rawCounts > caption > auto
	// hideSubvalue / showResetTimer follow the same plugin-tier
	// fallback chain as the other toggles: button > provider > plugin.
	// ks.* is nil when nothing at button/provider tier is set.
	hideSub := settings.DefaultHideSubvalueEnabled()
	if ks.HideSubvalue != nil {
		hideSub = *ks.HideSubvalue
	}
	showTimer := settings.DefaultShowResetTimerEnabled()
	if ks.ShowResetTimer != nil {
		showTimer = *ks.ShowResetTimer
	}
	if hideSub {
		// User explicitly hid the subtext.
	} else if metric.ResetInSeconds != nil && showTimer {
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
	// Bg: button override > plugin default > brand bg. Consistent
	// with the main render path.
	bg := prov.BrandBg()
	if v := settings.DefaultBgColorValue(); v != "" && render.IsValidHexColor(v) {
		bg = v
	}
	if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
		bg = ks.BgColor
	}
	// Fill: button/provider override > plugin default > brand. Reference
	// cards don't draw a fill (no ratio), but Fill still colors the
	// glyph watermark, so honoring the override keeps the placeholder
	// visually consistent with the live tile.
	fill := prov.BrandColor()
	if v := settings.DefaultFillColorValue(); v != "" && render.IsValidHexColor(v) {
		fill = v
	}
	if ks.FillColor != "" && render.IsValidHexColor(ks.FillColor) {
		fill = ks.FillColor
	}
	in := render.ButtonInput{
		Label:         label,
		Value:         value,
		Fill:          fill,
		Bg:            bg,
		SmartContrast: settings.SmartContrastFor(prov.ID()),
		Starfield:     settings.StarfieldEnabled(prov.ID(), ks),
	}
	if subvalue != "" {
		in.Subvalue = subvalue
	}
	if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
		in.Fg = ks.TextColor
	} else if v := settings.DefaultTextColorValue(); render.IsValidHexColor(v) {
		in.Fg = v
	}
	borderOn := settings.DefaultShowBorderEnabled()
	if ks.ShowBorder != nil {
		borderOn = *ks.ShowBorder
	}
	if !borderOn {
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
	// Fill: button/provider override > plugin default > brand.
	fillColor := ""
	if prov != nil {
		fillColor = prov.BrandColor()
	}
	if v := settings.DefaultFillColorValue(); v != "" && render.IsValidHexColor(v) {
		fillColor = v
	}
	// Bg: button override > plugin default > brand bg.
	bg := "#111827"
	if prov != nil {
		bg = prov.BrandBg()
	}
	if v := settings.DefaultBgColorValue(); v != "" && render.IsValidHexColor(v) {
		bg = v
	}
	fg := settings.DefaultTextColorValue()
	var border *bool
	if !settings.DefaultShowBorderEnabled() {
		f := false
		border = &f
	}
	if ks != nil {
		if ks.FillColor != "" && render.IsValidHexColor(ks.FillColor) {
			fillColor = ks.FillColor
		}
		if ks.BgColor != "" && render.IsValidHexColor(ks.BgColor) {
			bg = ks.BgColor
		}
		if ks.TextColor != "" && render.IsValidHexColor(ks.TextColor) {
			fg = ks.TextColor
		}
		if ks.ShowBorder != nil {
			border = ks.ShowBorder
		}
	}
	starfield := false
	if ks != nil {
		starfield = settings.StarfieldEnabled(providerID, *ks)
	} else {
		starfield = settings.StarfieldEnabled(providerID, settings.KeySettings{})
	}
	return render.RenderLoading(glyph, fillColor, bg, fg, border, starfield)
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

	// Per-metric-type built-in defaults. These fire only when neither
	// the button, provider, nor plugin tier has supplied a threshold.
	var defaultWarn, defaultCritical *float64
	if direction == "high" {
		w, c := 10.0, 0.0
		defaultWarn, defaultCritical = &w, &c
	} else if metric.NumericMax != nil && *metric.NumericMax > 0 {
		w := *metric.NumericMax * 0.8
		c := *metric.NumericMax
		defaultWarn, defaultCritical = &w, &c
	}

	// Precedence: button / provider merged into ks > plugin default >
	// per-metric-type built-in.
	warn := ks.WarnBelow
	if warn == nil {
		if v, ok := settings.DefaultWarnBelowValue(); ok {
			warn = &v
		} else {
			warn = defaultWarn
		}
	}
	critical := ks.CriticalBelow
	if critical == nil {
		if v, ok := settings.DefaultCriticalBelowValue(); ok {
			critical = &v
		} else {
			critical = defaultCritical
		}
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
	// Effective show-raw-counts value: button > provider > plugin.
	// ks.ShowRawCounts already carries the merged button/provider
	// value from EffectiveSettings; nil means "inherit plugin".
	effective := settings.DefaultShowRawCountsEnabled()
	explicit := ks.ShowRawCounts != nil
	if explicit {
		effective = *ks.ShowRawCounts
	}
	if explicit && !effective {
		return false
	}
	if metric.RawCount != nil && metric.RawMax != nil {
		return effective
	}
	if effective &&
		metric.NumericValue != nil && metric.NumericMax != nil &&
		metric.NumericUnit == "dollars" {
		return true
	}
	return false
}

// effectiveMetricID resolves the metric ID a key should display. When
// the key has no saved MetricID, picks the first entry from the
// provider's MetricIDs() so a freshly-dropped action lands on a metric
// that actually exists for that provider — not the Claude-flavored
// "session-percent" default. Falls back to defaultMetric when the
// provider isn't registered or has no metrics declared.
func effectiveMetricID(ks settings.KeySettings, providerID string) string {
	if ks.MetricID != "" {
		return ks.MetricID
	}
	if providerID != "" {
		if prov := providers.Get(providerID); prov != nil {
			if ids := prov.MetricIDs(); len(ids) > 0 {
				return ids[0]
			}
		}
	}
	return defaultMetric
}

func metricWithSnapshotAge(metric providers.MetricValue, snapshotAge time.Duration) providers.MetricValue {
	if metric.ResetInSeconds == nil || snapshotAge <= 0 {
		return metric
	}
	remaining := *metric.ResetInSeconds - snapshotAge.Seconds()
	if remaining < 0 {
		remaining = 0
	}
	metric.ResetInSeconds = &remaining
	return metric
}

func isCustomTitle(title string, autoTitles ...string) bool {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return false
	}
	for _, auto := range autoTitles {
		if auto != "" && strings.EqualFold(trimmed, strings.TrimSpace(auto)) {
			return false
		}
	}
	return true
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
	"session-percent":         "SESSION",
	"session-pace":            "SESSION",
	"weekly-percent":          "WEEKLY",
	"weekly-pace":             "WEEKLY",
	"weekly-sonnet-percent":   "SONNET",
	"weekly-opus-percent":     "OPUS",
	"weekly-design-percent":   "DESIGN",
	"weekly-routines-percent": "ROUTINES",
	"sonnet-pace":             "SONNET",
	"opus-pace":               "OPUS",
	"design-pace":             "DESIGN",
	"routines-pace":           "ROUTINES",
	"extra-usage-percent":     "EXTRA USAGE",
	"extra-usage-limit":       "LIMIT",
	"extra-usage-remaining":   "LEFT",
	"extra-usage-spent":       "SPENT",
	"extra-usage-balance":     "BALANCE",
	"extra-usage-enabled":     "EXTRA USAGE",
	"extra-usage-auto-reload": "RELOAD",
	"credits-balance":         "CREDITS",
	"credits":                 "CREDITS",
	"cost-today":              "TODAY",
	"cost-30d":                "30 DAYS",
	"tokens-session-percent":  "5-HOUR",
	"team-ondemand-spent":     "TEAM",
	// Grok / xAI: title identifies the model; the subvalue caption
	// disambiguates Queries / Tokens at a glance. (Grok 4 Heavy is
	// the only Grok 4 tier we track, so the title is plain "GROK 4".)
	"grok3-queries-remaining":       "GROK 3",
	"grok3-tokens-remaining":        "GROK 3",
	"grok4-heavy-queries-remaining": "GROK 4",
	// Perplexity: per-feature daily quotas + Comet/API dollar metrics.
	// Titles match the live-tile labels emitted from snapshotFromUsage
	// so a placeholder Perplexity tile reads identically to the live
	// one. api-balance / api-spend share the "API" title — caption
	// disambiguates (Balance vs Spend), same trick Grok uses.
	"pro-queries-remaining":      "PRO",
	"deep-research-remaining":    "DEEP RSRCH.",
	"labs-remaining":             "LABS",
	"agentic-research-remaining": "AGENTIC",
	"comet-spend":                "COMET",
	"api-balance":                "API",
	"api-spend":                  "API",
}

// metricCaptionForPlaceholder returns a short caption for dashed-out
// placeholder buttons (no data yet) so the user knows what the button
// is for even when the glyph is the only visual.
var knownCaptions = map[string]string{
	"session-percent":         "Remaining",
	"session-pace":            "Pace",
	"weekly-percent":          "Remaining",
	"weekly-pace":             "Pace",
	"weekly-sonnet-percent":   "Remaining",
	"weekly-opus-percent":     "Remaining",
	"weekly-design-percent":   "Remaining",
	"weekly-routines-percent": "Remaining",
	"sonnet-pace":             "Pace",
	"opus-pace":               "Pace",
	"design-pace":             "Pace",
	"routines-pace":           "Pace",
	"extra-usage-percent":     "Remaining",
	"extra-usage-limit":       "Monthly",
	"extra-usage-spent":       "Account total",
	"extra-usage-balance":     "Prepaid",
	"extra-usage-enabled":     "Toggle",
	"extra-usage-auto-reload": "Auto-reload",
	"credits-balance":         "Balance",
	"cost-today":              "Cost (local)",
	"cost-30d":                "Cost (local)",
	"tokens-session-percent":  "Remaining",
	"team-ondemand-spent":     "Team spend",
	// Grok placeholder captions match the live-tile subvalue so a
	// dashed-out Grok button still tells you which category it is.
	"grok3-queries-remaining":       "Queries",
	"grok3-tokens-remaining":        "Tokens",
	"grok4-heavy-queries-remaining": "Queries",
	// Perplexity placeholder captions mirror the live-tile subvalue —
	// constant "Queries" for the per-feature counts, "Balance"/"Spend"
	// for the dollar metrics so a row of Perplexity tiles reads as
	// parallel and api-balance vs api-spend stay disambiguated.
	"pro-queries-remaining":      "Queries",
	"deep-research-remaining":    "Queries",
	"labs-remaining":             "Queries",
	"agentic-research-remaining": "Queries",
	"comet-spend":                "Spend",
	"api-balance":                "Balance",
	"api-spend":                  "Spend",
}

func metricCaptionForPlaceholder(metricID string) string {
	if caption, ok := knownCaptions[metricID]; ok {
		return caption
	}
	switch {
	case strings.HasSuffix(metricID, "-percent"):
		return "Remaining"
	case strings.HasSuffix(metricID, "-pace"):
		return "Pace"
	}
	return ""
}

func deriveLabelFromMetricID(metricID string) string {
	if label, ok := knownLabels[metricID]; ok {
		return label
	}
	trimmed := strings.TrimPrefix(metricID, "weekly-")
	for _, suffix := range []string{"-percent", "-pace"} {
		if strings.HasSuffix(trimmed, suffix) {
			trimmed = strings.TrimSuffix(trimmed, suffix)
			break
		}
	}
	return strings.ToUpper(strings.ReplaceAll(trimmed, "-", " "))
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
	notConfiguredRe = regexp.MustCompile(`(?i)not.?configured|not found|Set \w+_\w+|Paste a Cookie|Install the Usage Buttons|No .+ token found`)
	expiredRe       = regexp.MustCompile(`(?i)expired|unauthorized|re-authenticate`)
	signedOutRe     = regexp.MustCompile(`(?i)signed out|session is signed out`)
	scopeRe         = regexp.MustCompile(`(?i)missing.*scope`)
	// extensionTimeoutRe matches loopback (sidecar ↔ plugin) timeouts —
	// e.g. "read tcp 127.0.0.1:... i/o timeout". The fetch never left
	// the machine, so calling this a "network error" is misleading.
	extensionTimeoutRe = regexp.MustCompile(`(?i)(127\.0\.0\.1|extension response timeout).*(timeout|i/o)`)
	// extensionNeededRe matches the "not configured" family of errors
	// that specifically need the Chrome extension (cookie-gated
	// providers) rather than an API key.
	extensionNeededRe = regexp.MustCompile(`(?i)Install the Usage Buttons|Paste a Cookie|Helper Chrome extension`)
	networkRe         = regexp.MustCompile(`(?i)network error|dial tcp|connection refused|i/o timeout|context deadline exceeded|ETIMEDOUT`)
	serverErrRe       = regexp.MustCompile(`(?i)server error|HTTP [5]\d\d`)
)

func isRateLimit(msg string) bool        { return rateLimitRe.MatchString(msg) }
func isNotConfigured(msg string) bool    { return notConfiguredRe.MatchString(msg) }
func isExpired(msg string) bool          { return expiredRe.MatchString(msg) }
func isSignedOut(msg string) bool        { return signedOutRe.MatchString(msg) }
func isMissingScope(msg string) bool     { return scopeRe.MatchString(msg) }
func isExtensionTimeout(msg string) bool { return extensionTimeoutRe.MatchString(msg) }
func isExtensionNeeded(msg string) bool  { return extensionNeededRe.MatchString(msg) }
func isNetworkError(msg string) bool     { return networkRe.MatchString(msg) }
func isServerError(msg string) bool      { return serverErrRe.MatchString(msg) }

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
