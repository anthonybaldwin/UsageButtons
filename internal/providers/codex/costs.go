package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
	"github.com/anthonybaldwin/UsageButtons/internal/wsl"
)

// Codex CLI writes one JSONL file per session under
//   ~/.codex/sessions/YYYY/MM/DD/rollout-...-<sessionId>.jsonl
//
// Each line is an event. The first line is typically a session_meta
// event carrying the session_id and, for forked sessions, forked_from_id
// plus a fork timestamp. Token accounting comes from events of the form
//   {"type":"event_msg","payload":{"type":"token_count","info":{...}}}
// where info.total_token_usage is the cumulative-since-session-start
// input / cached_input / output / reasoning_output token count.
//
// To bill correctly we compute per-event deltas (current cumulative minus
// previous cumulative) and bucket each delta by its own timestamp. For
// forked sessions, we subtract the parent's cumulative at fork time as
// the baseline so inherited tokens are not double-counted.

var (
	// codexCostMu guards the cost cache against concurrent scanners.
	codexCostMu sync.Mutex
	// codexCostCache is the most recent scan result, including any
	// running WSL distros on Windows.
	codexCostCache *allCodexCostResults
	// codexCostCacheT is the time of the most recent scan.
	codexCostCacheT time.Time
	// codexCostCacheErr is the error from the most recent native scan.
	codexCostCacheErr error
)

// codexCostCacheTTL bounds how often session logs are rescanned.
const codexCostCacheTTL = 5 * time.Minute

// codexCumulativeBaselineMinTokens is the smallest inherited baseline that
// can be treated as cumulative without a pinned bucket signal.
//
// Small single-bucket baselines can be exceeded by ordinary per-turn rows; a
// larger inherited baseline is more likely to represent cumulative fork state
// when every bucket covers the baseline but none is pinned.
const codexCumulativeBaselineMinTokens = 1_000

// codexPricing stores per-million-token prices in USD.
type codexPricing struct {
	input  float64
	output float64
	cached *float64
}

// codexPriceTable mirrors CodexBar's Codex model pricing table. Unknown
// models intentionally do not fall back to a fake cost; we keep the local
// estimate limited to models whose prices we know.
var codexPriceTable = map[string]codexPricing{
	"gpt-5":               {input: 1.25, output: 10.0, cached: ptrFloat(0.125)},
	"gpt-5-codex":         {input: 1.25, output: 10.0, cached: ptrFloat(0.125)},
	"gpt-5-mini":          {input: 0.25, output: 2.0, cached: ptrFloat(0.025)},
	"gpt-5-nano":          {input: 0.05, output: 0.4, cached: ptrFloat(0.005)},
	"gpt-5-pro":           {input: 15.0, output: 120.0},
	"gpt-5.1":             {input: 1.25, output: 10.0, cached: ptrFloat(0.125)},
	"gpt-5.1-codex":       {input: 1.25, output: 10.0, cached: ptrFloat(0.125)},
	"gpt-5.1-codex-max":   {input: 1.25, output: 10.0, cached: ptrFloat(0.125)},
	"gpt-5.1-codex-mini":  {input: 0.25, output: 2.0, cached: ptrFloat(0.025)},
	"gpt-5.2":             {input: 1.75, output: 14.0, cached: ptrFloat(0.175)},
	"gpt-5.2-codex":       {input: 1.75, output: 14.0, cached: ptrFloat(0.175)},
	"gpt-5.2-pro":         {input: 21.0, output: 168.0},
	"gpt-5.3-codex":       {input: 1.75, output: 14.0, cached: ptrFloat(0.175)},
	"gpt-5.3-codex-spark": {input: 0, output: 0, cached: ptrFloat(0)},
	"gpt-5.4":             {input: 2.5, output: 15.0, cached: ptrFloat(0.25)},
	"gpt-5.4-mini":        {input: 0.75, output: 4.5, cached: ptrFloat(0.075)},
	"gpt-5.4-nano":        {input: 0.2, output: 1.25, cached: ptrFloat(0.02)},
	"gpt-5.4-pro":         {input: 30.0, output: 180.0},
	"gpt-5.5":             {input: 5.0, output: 30.0, cached: ptrFloat(0.5)},
	"gpt-5.5-pro":         {input: 30.0, output: 180.0},
}

// codexTokenUsage mirrors the total_token_usage object Codex writes into
// every token_count event.
type codexTokenUsage struct {
	// InputTokens is the uncached input token count.
	InputTokens int `json:"input_tokens"`
	// CachedInputTokens is the cached input token count.
	CachedInputTokens int `json:"cached_input_tokens"`
	// CacheReadInputTokens is the alternate cached input token count key.
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	// OutputTokens is the visible output token count.
	OutputTokens int `json:"output_tokens"`
	// ReasoningOutputTokens is the hidden reasoning output token count.
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	// TotalTokens is Codex's optional aggregate token count.
	TotalTokens int `json:"total_tokens"`
}

// codexLine covers both session_meta and event_msg/token_count lines.
// Unused fields for a given event type stay zero-valued.
type codexLine struct {
	// Timestamp is the event timestamp string.
	Timestamp string `json:"timestamp"`
	// Type is the event type.
	Type string `json:"type"`
	// Model is a top-level model hint.
	Model string `json:"model"`
	// Payload is the event body.
	Payload *struct {
		// Type is the payload event type.
		Type string `json:"type"`
		// SessionID is the Codex session identifier.
		SessionID string `json:"session_id"`
		// ForkedFromID is the snake_case parent session identifier.
		ForkedFromID string `json:"forked_from_id"`
		// ForkedFromIDAlt is the camelCase parent session identifier.
		ForkedFromIDAlt string `json:"forkedFromId"`
		// ParentSessionID is the alternate parent session identifier.
		ParentSessionID string `json:"parent_session_id"`
		// Timestamp is the payload timestamp string.
		Timestamp string `json:"timestamp"`
		// Model is a payload-level model hint.
		Model string `json:"model"`
		// Info contains token usage and model metadata.
		Info *struct {
			// TotalTokenUsage is cumulative usage for the session.
			TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
			// LastTokenUsage is usage for the last turn or cumulative fork row.
			LastTokenUsage *codexTokenUsage `json:"last_token_usage"`
			// Model is an info-level model hint.
			Model string `json:"model"`
			// ModelName is an alternate info-level model hint.
			ModelName string `json:"model_name"`
		} `json:"info"`
	} `json:"payload"`
}

// codexCostResult aggregates today / last-30d token cost estimates.
type codexCostResult struct {
	// Today is the estimated spend since local midnight.
	Today float64
	// Last30d is the estimated spend over the last 30 days.
	Last30d float64
	// Seen reports whether at least one priced Codex usage row was found.
	Seen bool
}

// codexRawSnapshot is one cumulative total_token_usage observation
// paired with the timestamp at which it was emitted.
type codexRawSnapshot struct {
	ts    time.Time
	usage codexTokenUsage
}

// codexUsageDelta is a token delta paired with the model active when it
// was emitted.
type codexUsageDelta struct {
	ts    time.Time
	model string
	usage codexTokenUsage
}

// codexSessionMeta captures the session-identifying fields needed to
// handle forked sessions correctly.
type codexSessionMeta struct {
	sessionID    string
	forkedFromID string
	forkTS       time.Time
}

// codexScanState threads session-ID lookups and parent-snapshot caches
// through a single scan so forked sessions reparse parents at most once.
type codexScanState struct {
	byID        map[string]string // session ID → file path, populated during discovery
	metaByID    map[string]codexSessionMeta
	parentCache map[string][]codexRawSnapshot
}

// codexCostLog emits a tagged local-cost scan warning through the provider log sink.
func codexCostLog(format string, args ...any) {
	if providers.LogSink != nil {
		providers.LogSink(fmt.Sprintf("[codex] local costs: "+format, args...))
	}
}

// sessionsRoot returns the Windows-native filesystem root under which
// Codex writes session JSONL files, honoring CODEX_HOME when set.
//
// WSL distros are scanned via scanCodexCosts using their own home paths;
// CODEX_HOME is intentionally NOT propagated into WSL scopes since each
// distro is treated as a separate machine with its own environment.
func sessionsRoot() string {
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		return filepath.Join(ch, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// allCodexCostResults holds the codexCostResult for the Windows-native
// session tree plus, on Windows builds with WSL distros running, one
// codexCostResult per running distro keyed by wsl.Source.Key. Each WSL
// scope is treated as a separate machine — never aggregated with native.
type allCodexCostResults struct {
	Native codexCostResult
	// WSL is keyed by wsl.Source.Key. Empty/nil when WSL is unavailable
	// or no distros are running.
	WSL map[string]codexCostResult
	// WSLLabels maps Source.Key → friendly distro name for UI use.
	WSLLabels map[string]string
}

// scanCodexCosts walks the Windows-native session tree plus the
// equivalent path inside every running WSL distro, returning per-scope
// aggregates memoized for codexCostCacheTTL. The returned error reflects
// only the native scan; failed WSL scopes are silently skipped so a
// flaky distro can't poison the Windows tile.
func scanCodexCosts() (*allCodexCostResults, error) {
	codexCostMu.Lock()
	defer codexCostMu.Unlock()
	if codexCostCache != nil && time.Since(codexCostCacheT) < codexCostCacheTTL {
		return codexCostCache, codexCostCacheErr
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	out := &allCodexCostResults{}
	native, nativeErr := scanCodexSessionsTree(sessionsRoot(), todayStart, thirtyDaysAgo)
	out.Native = native

	if sources := wsl.Sources(); len(sources) > 0 {
		out.WSL = make(map[string]codexCostResult, len(sources))
		out.WSLLabels = make(map[string]string, len(sources))
		for _, src := range sources {
			r, err := scanCodexSessionsTree(filepath.Join(src.Home, ".codex", "sessions"), todayStart, thirtyDaysAgo)
			if err != nil {
				codexCostLog("WSL %s scan failed: %v", src.Label, err)
				continue
			}
			out.WSL[src.Key] = r
			out.WSLLabels[src.Key] = src.Label
		}
	}

	codexCostCacheErr = nativeErr
	codexCostCache = out
	codexCostCacheT = time.Now()
	return codexCostCache, codexCostCacheErr
}

// scanCodexSessionsTree walks one Codex sessions root (native or
// \\wsl.localhost\...) and returns the aggregated token-cost estimate.
// A missing root returns a zero result without error so callers can
// scan optional scopes safely.
func scanCodexSessionsTree(root string, todayStart, thirtyDaysAgo time.Time) (codexCostResult, error) {
	var result codexCostResult
	if root == "" {
		return result, nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}

	state := &codexScanState{
		byID:        make(map[string]string),
		metaByID:    make(map[string]codexSessionMeta),
		parentCache: make(map[string][]codexRawSnapshot),
	}

	type fileEntry struct {
		path string
		meta codexSessionMeta
	}
	var toScan []fileEntry

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			codexCostLog("skipping %s: %v", path, err)
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			codexCostLog("skipping %s: %v", path, err)
			return nil
		}
		meta, _, err := readCodexSessionMeta(path)
		if err != nil {
			codexCostLog("skipping %s: %v", path, err)
			return nil
		}
		if meta.sessionID != "" {
			state.byID[meta.sessionID] = path
			state.metaByID[meta.sessionID] = meta
		}
		// Mod-time is a cheap pre-filter; a fork may still need the
		// file as a parent even if its own last event is out of window,
		// but byID is already populated above.
		if info.ModTime().Before(thirtyDaysAgo) {
			return nil
		}
		toScan = append(toScan, fileEntry{path: path, meta: meta})
		return nil
	})

	for _, fe := range toScan {
		if scanErr := scanCodexSessionFile(state, fe.path, fe.meta, todayStart, thirtyDaysAgo, &result); scanErr != nil {
			codexCostLog("skipping %s: %v", fe.path, scanErr)
		}
	}

	return result, walkErr
}

// readCodexSessionMeta reads the first handful of lines of a JSONL file
// looking for the session_meta event. Returns an empty meta if not found.
func readCodexSessionMeta(path string) (codexSessionMeta, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return codexSessionMeta{}, false, fmt.Errorf("open codex session meta %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	for i := 0; i < 20 && scanner.Scan(); i++ {
		var ev codexLine
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "session_meta" {
			continue
		}
		p := ev.Payload
		meta := codexSessionMeta{}
		if p != nil {
			meta.sessionID = p.SessionID
			meta.forkedFromID = firstNonEmpty(p.ForkedFromID, p.ForkedFromIDAlt, p.ParentSessionID)
			// Fork timestamp is the payload's own timestamp (when the
			// fork was created); fall back to the line timestamp.
			if t, ok := parseCodexTimestamp(p.Timestamp); ok {
				meta.forkTS = t
			}
		}
		if meta.forkTS.IsZero() {
			if t, ok := parseCodexTimestamp(ev.Timestamp); ok {
				meta.forkTS = t
			}
		}
		return meta, true, nil
	}
	if err := scanner.Err(); err != nil {
		return codexSessionMeta{}, false, fmt.Errorf("scan codex session meta %s: %w", path, err)
	}
	return codexSessionMeta{}, false, nil
}

// parentSnapshots returns every cumulative total_token_usage in the named
// parent session, in timestamp order. Results are cached on the scan state
// so multiple children forking from the same parent don't reparse it.
func parentSnapshots(state *codexScanState, parentID string) ([]codexRawSnapshot, bool) {
	if snaps, ok := state.parentCache[parentID]; ok {
		return snaps, true
	}
	path, ok := state.byID[parentID]
	if !ok {
		return nil, false
	}
	var seed codexTokenUsage
	if meta := state.metaByID[parentID]; meta.forkedFromID != "" {
		var ok bool
		seed, ok = inheritedBaseline(state, meta.forkedFromID, meta.forkTS)
		if !ok {
			return nil, false
		}
	}
	snaps, err := readCodexSnapshots(path, seed)
	if err != nil {
		return nil, false
	}
	state.parentCache[parentID] = snaps
	return snaps, true
}

// readCodexSnapshots reads every cumulative total_token_usage from a file
// in the order it appears. It reads the raw cumulative values — fork
// inheritance subtraction is applied separately where needed.
func readCodexSnapshots(path string, seed codexTokenUsage) ([]codexRawSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open codex snapshots %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var out []codexRawSnapshot
	prev := seed
	remainingInherited := &seed
	for scanner.Scan() {
		var ev codexLine
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "event_msg" || ev.Payload == nil || ev.Payload.Type != "token_count" {
			continue
		}
		if ev.Payload.Info == nil {
			continue
		}
		t, ok := parseCodexTimestamp(ev.Timestamp)
		if !ok {
			continue
		}
		switch {
		case ev.Payload.Info.TotalTokenUsage != nil:
			prev = normalizeCodexUsage(*ev.Payload.Info.TotalTokenUsage)
			remainingInherited = nil
		case ev.Payload.Info.LastTokenUsage != nil:
			delta := adjustInheritedLastDelta(normalizeCodexUsage(*ev.Payload.Info.LastTokenUsage), &remainingInherited)
			prev = addUsage(prev, delta)
		default:
			continue
		}
		out = append(out, codexRawSnapshot{ts: t, usage: prev})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan codex snapshots %s: %w", path, err)
	}
	return out, nil
}

// inheritedBaseline walks the parent's snapshots and returns the last
// cumulative usage at or before the fork timestamp. For recursive forks,
// the parent's cumulative already embeds its own inheritance, which is
// what we want: child's raw first event minus parent's raw cumulative
// yields exactly the tokens the child added.
func inheritedBaseline(state *codexScanState, parentID string, forkTS time.Time) (codexTokenUsage, bool) {
	var baseline codexTokenUsage
	snaps, ok := parentSnapshots(state, parentID)
	if !ok {
		return codexTokenUsage{}, false
	}
	for _, s := range snaps {
		if s.ts.After(forkTS) {
			break
		}
		baseline = s.usage
	}
	return baseline, true
}

// scanCodexSessionFile walks one rollout JSONL, computes per-event deltas
// from the raw cumulative totals, and buckets each delta into today /
// last-30d by its own timestamp (not the session's final timestamp).
func scanCodexSessionFile(state *codexScanState, path string, meta codexSessionMeta, todayStart, thirtyDaysAgo time.Time, result *codexCostResult) error {
	deltas, err := readCodexDeltas(path, func(parentID string, forkTS time.Time) (codexTokenUsage, bool) {
		return inheritedBaseline(state, parentID, forkTS)
	}, meta)
	if err != nil {
		return err
	}
	if len(deltas) == 0 {
		return nil
	}

	for _, d := range deltas {
		if d.ts.Before(thirtyDaysAgo) {
			continue
		}
		cost, ok := codexTokenCost(d.model, d.usage)
		if !ok {
			continue
		}
		result.Seen = true
		result.Last30d += cost
		if !d.ts.Before(todayStart) {
			result.Today += cost
		}
	}
	return nil
}

// readCodexDeltas parses one session log into per-event token deltas with
// the best model hint available for each token_count event.
func readCodexDeltas(path string, inherited func(string, time.Time) (codexTokenUsage, bool), meta codexSessionMeta) ([]codexUsageDelta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open codex deltas %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var out []codexUsageDelta
	var currentModel string
	var pending []codexUsageDelta
	var remainingInherited *codexTokenUsage
	var prev codexTokenUsage
	if meta.forkedFromID != "" && inherited != nil {
		// Fall back to a zero baseline when the parent is not resolvable
		// (parent file deleted, permission flipped mid-walk, or sat under a
		// directory that errored during discovery). Pricing the child from
		// zero over-counts inherited tokens, but that is preferable to
		// dropping the entire session's cost data for the TTL window.
		if baseline, ok := inherited(meta.forkedFromID, meta.forkTS); ok {
			prev = baseline
			remainingInherited = &baseline
		} else {
			codexCostLog("parent session %q not found; pricing %s from zero baseline", meta.forkedFromID, path)
		}
	}

	for scanner.Scan() {
		var ev codexLine
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if model := codexModelHint(ev); model != "" {
			currentModel = normalizeCodexModel(model)
			for i := range pending {
				pending[i].model = currentModel
			}
			out = append(out, pending...)
			pending = nil
		}
		if ev.Type != "event_msg" || ev.Payload == nil || ev.Payload.Type != "token_count" {
			continue
		}
		t, ok := parseCodexTimestamp(ev.Timestamp)
		if !ok {
			continue
		}
		if ev.Payload.Info == nil {
			continue
		}

		var delta codexTokenUsage
		if ev.Payload.Info.TotalTokenUsage != nil {
			total := normalizeCodexUsage(*ev.Payload.Info.TotalTokenUsage)
			delta = subUsage(total, prev)
			prev = total
			remainingInherited = nil
		} else if ev.Payload.Info.LastTokenUsage != nil {
			delta = adjustInheritedLastDelta(normalizeCodexUsage(*ev.Payload.Info.LastTokenUsage), &remainingInherited)
			prev = addUsage(prev, delta)
		} else {
			continue
		}
		if isZeroUsage(delta) {
			continue
		}
		d := codexUsageDelta{ts: t, model: currentModel, usage: delta}
		if currentModel == "" {
			// Keep model-less codexUsageDelta rows in pending until a
			// currentModel hint can flush them into out; if none arrives, they
			// are discarded rather than priced as an unknown model.
			pending = append(pending, d)
			continue
		}
		out = append(out, d)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan codex deltas %s: %w", path, err)
	}
	return out, nil
}

// adjustInheritedLastDelta subtracts inherited fork tokens from
// last_token_usage rows only when the row clearly contains the full inherited
// baseline plus new usage. Plain per-turn rows are returned unchanged.
func adjustInheritedLastDelta(raw codexTokenUsage, remaining **codexTokenUsage) codexTokenUsage {
	if remaining == nil || *remaining == nil {
		return raw
	}
	baseline := **remaining
	if !looksCumulativeInheritedUsage(raw, baseline) {
		*remaining = nil
		return raw
	}
	adjusted := subUsage(raw, baseline)
	*remaining = nil
	return adjusted
}

// looksCumulativeInheritedUsage reports whether raw looks like a cumulative
// inherited-plus-new row rather than a coincidentally large per-turn row.
func looksCumulativeInheritedUsage(raw, baseline codexTokenUsage) bool {
	return coversUsage(raw, baseline) && hasCumulativeShapeSignal(raw, baseline)
}

// coversUsage reports whether raw includes every non-zero token bucket in
// baseline, which indicates inherited-plus-new rather than per-turn usage.
func coversUsage(raw, baseline codexTokenUsage) bool {
	return coversTokenBucket(raw.InputTokens, baseline.InputTokens) &&
		coversTokenBucket(logicalCachedTokens(raw), logicalCachedTokens(baseline)) &&
		coversTokenBucket(raw.OutputTokens, baseline.OutputTokens) &&
		coversTokenBucket(raw.ReasoningOutputTokens, baseline.ReasoningOutputTokens)
}

// coversTokenBucket reports whether raw contains a non-zero baseline bucket.
func coversTokenBucket(raw, baseline int) bool {
	return baseline <= 0 || raw >= baseline
}

// hasCumulativeShapeSignal reports whether raw has enough baseline shape to
// avoid treating a single-bucket per-turn row as cumulative inherited usage.
func hasCumulativeShapeSignal(raw, baseline codexTokenUsage) bool {
	if pinnedTokenBucket(raw.InputTokens, baseline.InputTokens) ||
		pinnedTokenBucket(logicalCachedTokens(raw), logicalCachedTokens(baseline)) ||
		pinnedTokenBucket(raw.OutputTokens, baseline.OutputTokens) ||
		pinnedTokenBucket(raw.ReasoningOutputTokens, baseline.ReasoningOutputTokens) {
		return true
	}
	return billableUsageTokens(baseline) >= codexCumulativeBaselineMinTokens
}

// pinnedTokenBucket reports whether a non-zero inherited bucket is unchanged.
func pinnedTokenBucket(raw, baseline int) bool {
	return baseline > 0 && raw == baseline
}

// logicalCachedTokens treats cached_input_tokens/cache_read_input_tokens as aliases.
func logicalCachedTokens(u codexTokenUsage) int {
	return max(u.CachedInputTokens, u.CacheReadInputTokens)
}

// billableUsageTokens totals logical billable buckets, excluding total_tokens.
func billableUsageTokens(u codexTokenUsage) int {
	return maxZero(u.InputTokens) +
		maxZero(logicalCachedTokens(u)) +
		maxZero(u.OutputTokens) +
		maxZero(u.ReasoningOutputTokens)
}

// subUsage subtracts b from a field-by-field. Negative fields are floored
// to zero — total_token_usage should monotonically grow within a session,
// but defensive clamping guards against log anomalies.
func subUsage(a, b codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		InputTokens:           maxZero(a.InputTokens - b.InputTokens),
		CachedInputTokens:     maxZero(a.CachedInputTokens - b.CachedInputTokens),
		CacheReadInputTokens:  maxZero(a.CacheReadInputTokens - b.CacheReadInputTokens),
		OutputTokens:          maxZero(a.OutputTokens - b.OutputTokens),
		ReasoningOutputTokens: maxZero(a.ReasoningOutputTokens - b.ReasoningOutputTokens),
		TotalTokens:           maxZero(a.TotalTokens - b.TotalTokens),
	}
}

// addUsage adds b to a field-by-field.
func addUsage(a, b codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		InputTokens:           a.InputTokens + b.InputTokens,
		CachedInputTokens:     a.CachedInputTokens + b.CachedInputTokens,
		CacheReadInputTokens:  a.CacheReadInputTokens + b.CacheReadInputTokens,
		OutputTokens:          a.OutputTokens + b.OutputTokens,
		ReasoningOutputTokens: a.ReasoningOutputTokens + b.ReasoningOutputTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
	}
}

// isZeroUsage reports whether u has no billable token movement.
func isZeroUsage(u codexTokenUsage) bool {
	return u.InputTokens == 0 &&
		u.CachedInputTokens == 0 &&
		u.CacheReadInputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.ReasoningOutputTokens == 0
}

// normalizeCodexUsage accepts both cached_input_tokens and the older
// cache_read_input_tokens spelling.
func normalizeCodexUsage(u codexTokenUsage) codexTokenUsage {
	if u.CachedInputTokens == 0 && u.CacheReadInputTokens > 0 {
		u.CachedInputTokens = u.CacheReadInputTokens
	}
	if u.CacheReadInputTokens == 0 && u.CachedInputTokens > 0 {
		u.CacheReadInputTokens = u.CachedInputTokens
	}
	return u
}

// maxZero clamps n to a non-negative int.
func maxZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// parseCodexTimestamp parses an RFC3339 or RFC3339Nano timestamp from a
// session log event, returning ok=false on unrecognised input.
func parseCodexTimestamp(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// codexTokenCost returns the USD cost for a single codexTokenUsage delta.
// The model argument must already be normalized via normalizeCodexModel.
// Free/preview models (all prices zero) return ok=false so zero-cost usage
// does not mark the session as billable and surface a misleading $0.00.
func codexTokenCost(model string, u codexTokenUsage) (float64, bool) {
	pricing, ok := codexPriceTable[model]
	if !ok {
		return 0, false
	}
	cached := min(maxZero(u.CachedInputTokens), maxZero(u.InputTokens))
	nonCached := maxZero(u.InputTokens - cached)
	cachedRate := pricing.input
	if pricing.cached != nil {
		cachedRate = *pricing.cached
	}
	cost := float64(nonCached)*pricing.input/1_000_000 +
		float64(cached)*cachedRate/1_000_000 +
		float64(maxZero(u.OutputTokens)+maxZero(u.ReasoningOutputTokens))*pricing.output/1_000_000
	if cost == 0 {
		return 0, false
	}
	return cost, true
}

// codexModelHint extracts a model hint from the known Codex JSONL shapes.
func codexModelHint(ev codexLine) string {
	if ev.Payload != nil {
		if ev.Payload.Info != nil {
			if ev.Payload.Info.Model != "" {
				return ev.Payload.Info.Model
			}
			if ev.Payload.Info.ModelName != "" {
				return ev.Payload.Info.ModelName
			}
		}
		if ev.Payload.Model != "" {
			return ev.Payload.Model
		}
	}
	return ev.Model
}

// normalizeCodexModel maps provider-prefixed and dated model IDs onto
// pricing-table keys.
func normalizeCodexModel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "openai/")
	if _, ok := codexPriceTable[trimmed]; ok {
		return trimmed
	}
	parts := strings.Split(trimmed, "-")
	if len(parts) > 3 {
		last := strings.Join(parts[len(parts)-3:], "-")
		if _, err := time.Parse("2006-01-02", last); err == nil {
			base := strings.Join(parts[:len(parts)-3], "-")
			if _, ok := codexPriceTable[base]; ok {
				return base
			}
		}
	}
	return trimmed
}

// ptrFloat returns a pointer to v.
func ptrFloat(v float64) *float64 {
	return &v
}

// codexCostMetrics returns cost-today + cost-30d metrics built from the
// local session-log scan. The Windows-native scope produces the stable
// "cost-today" / "cost-30d" IDs; each running WSL distro produces a
// parallel pair suffixed with the distro key, treated as a separate
// machine. Scopes with no priced rows are silently dropped so empty
// distros don't render a fake $0.00.
func codexCostMetrics() []providers.MetricValue {
	result, err := scanCodexCosts()
	if err != nil || result == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var out []providers.MetricValue

	emit := func(scopeSuffix, captionSuffix string, r codexCostResult) {
		if !r.Seen {
			return
		}
		today := math.Round(r.Today*100) / 100
		last30 := math.Round(r.Last30d*100) / 100
		out = append(out,
			providers.MetricValue{
				ID:              "cost-today" + scopeSuffix,
				Label:           "TODAY",
				Name:            "Estimated Codex spend today (local logs)" + captionSuffix,
				Value:           fmt.Sprintf("$%.2f", today),
				NumericValue:    &today,
				NumericUnit:     "dollars",
				NumericGoodWhen: "low",
				Caption:         "Cost (local)" + captionSuffix,
				UpdatedAt:       now,
			},
			providers.MetricValue{
				ID:              "cost-30d" + scopeSuffix,
				Label:           "30 DAYS",
				Name:            "Estimated Codex spend last 30 days (local logs)" + captionSuffix,
				Value:           fmt.Sprintf("$%.2f", last30),
				NumericValue:    &last30,
				NumericUnit:     "dollars",
				NumericGoodWhen: "low",
				Caption:         "Cost (local)" + captionSuffix,
				UpdatedAt:       now,
			},
		)
	}

	emit("", "", result.Native)
	for key, r := range result.WSL {
		label := result.WSLLabels[key]
		if label == "" {
			label = key
		}
		emit("-wsl-"+key, " (WSL: "+label+")", r)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}
