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
	codexCostMu       sync.Mutex
	codexCostCache    *codexCostResult
	codexCostCacheT   time.Time
	codexCostCacheErr error
)

const codexCostCacheTTL = 5 * time.Minute

// Per-million-token pricing for GPT-5 class Codex models (USD).
const (
	codexPriceInputPerMTok     = 1.25
	codexPriceCachedPerMTok    = 0.125
	codexPriceOutputPerMTok    = 10.0
	codexPriceReasoningPerMTok = 10.0
)

type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// codexLine covers both session_meta and event_msg/token_count lines.
// Unused fields for a given event type stay zero-valued.
type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   *struct {
		Type            string `json:"type"`
		SessionID       string `json:"session_id"`
		ForkedFromID    string `json:"forked_from_id"`
		ForkedFromIDAlt string `json:"forkedFromId"`
		ParentSessionID string `json:"parent_session_id"`
		Timestamp       string `json:"timestamp"`
		Info            *struct {
			TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

type codexCostResult struct {
	Today   float64
	Last30d float64
}

type codexRawSnapshot struct {
	ts    time.Time
	usage codexTokenUsage
}

type codexSessionMeta struct {
	sessionID    string
	forkedFromID string
	forkTS       time.Time
}

type codexScanState struct {
	byID        map[string]string // session ID → file path, populated during discovery
	parentCache map[string][]codexRawSnapshot
}

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

func scanCodexCosts() (*codexCostResult, error) {
	codexCostMu.Lock()
	defer codexCostMu.Unlock()
	if codexCostCache != nil && time.Since(codexCostCacheT) < codexCostCacheTTL {
		return codexCostCache, codexCostCacheErr
	}

	root := sessionsRoot()
	if root == "" {
		codexCostCacheErr = nil
		codexCostCache = &codexCostResult{}
		codexCostCacheT = time.Now()
		return codexCostCache, nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		codexCostCacheErr = nil
		codexCostCache = &codexCostResult{}
		codexCostCacheT = time.Now()
		return codexCostCache, nil
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	state := &codexScanState{
		byID:        make(map[string]string),
		parentCache: make(map[string][]codexRawSnapshot),
	}

	type fileEntry struct {
		path string
		meta codexSessionMeta
	}
	var toScan []fileEntry

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		meta, _ := readCodexSessionMeta(path)
		if meta.sessionID != "" {
			state.byID[meta.sessionID] = path
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

	var result codexCostResult
	for _, fe := range toScan {
		scanCodexSessionFile(state, fe.path, fe.meta, todayStart, thirtyDaysAgo, &result)
	}

	codexCostCacheErr = err
	codexCostCache = &result
	codexCostCacheT = time.Now()
	return codexCostCache, codexCostCacheErr
}

// readCodexSessionMeta reads the first handful of lines of a JSONL file
// looking for the session_meta event. Returns an empty meta if not found
// or the file can't be opened.
func readCodexSessionMeta(path string) (codexSessionMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexSessionMeta{}, false
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
		return meta, true
	}
	return codexSessionMeta{}, false
}

// parentSnapshots returns every cumulative total_token_usage in the named
// parent session, in timestamp order. Results are cached on the scan state
// so multiple children forking from the same parent don't reparse it.
func parentSnapshots(state *codexScanState, parentID string) []codexRawSnapshot {
	if snaps, ok := state.parentCache[parentID]; ok {
		return snaps
	}
	path, ok := state.byID[parentID]
	if !ok {
		state.parentCache[parentID] = nil
		return nil
	}
	snaps := readCodexSnapshots(path)
	state.parentCache[parentID] = snaps
	return snaps
}

// readCodexSnapshots reads every cumulative total_token_usage from a file
// in the order it appears. It reads the raw cumulative values — fork
// inheritance subtraction is applied separately where needed.
func readCodexSnapshots(path string) []codexRawSnapshot {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var out []codexRawSnapshot
	for scanner.Scan() {
		var ev codexLine
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "event_msg" || ev.Payload == nil || ev.Payload.Type != "token_count" {
			continue
		}
		if ev.Payload.Info == nil || ev.Payload.Info.TotalTokenUsage == nil {
			continue
		}
		t, ok := parseCodexTimestamp(ev.Timestamp)
		if !ok {
			continue
		}
		out = append(out, codexRawSnapshot{ts: t, usage: *ev.Payload.Info.TotalTokenUsage})
	}
	return out
}

// inheritedBaseline walks the parent's snapshots and returns the last
// cumulative usage at or before the fork timestamp. For recursive forks,
// the parent's cumulative already embeds its own inheritance, which is
// what we want: child's raw first event minus parent's raw cumulative
// yields exactly the tokens the child added.
func inheritedBaseline(state *codexScanState, parentID string, forkTS time.Time) codexTokenUsage {
	var baseline codexTokenUsage
	for _, s := range parentSnapshots(state, parentID) {
		if s.ts.After(forkTS) {
			break
		}
		baseline = s.usage
	}
	return baseline
}

// scanCodexSessionFile walks one rollout JSONL, computes per-event deltas
// from the raw cumulative totals, and buckets each delta into today /
// last-30d by its own timestamp (not the session's final timestamp).
func scanCodexSessionFile(state *codexScanState, path string, meta codexSessionMeta, todayStart, thirtyDaysAgo time.Time, result *codexCostResult) {
	snaps := readCodexSnapshots(path)
	if len(snaps) == 0 {
		return
	}

	var prev codexTokenUsage
	if meta.forkedFromID != "" {
		prev = inheritedBaseline(state, meta.forkedFromID, meta.forkTS)
	}

	for _, s := range snaps {
		delta := subUsage(s.usage, prev)
		prev = s.usage
		if s.ts.Before(thirtyDaysAgo) {
			continue
		}
		cost := codexTokenCost(delta)
		result.Last30d += cost
		if !s.ts.Before(todayStart) {
			result.Today += cost
		}
	}
}

// subUsage subtracts b from a field-by-field. Negative fields are floored
// to zero — total_token_usage should monotonically grow within a session,
// but defensive clamping guards against log anomalies.
func subUsage(a, b codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		InputTokens:           maxZero(a.InputTokens - b.InputTokens),
		CachedInputTokens:     maxZero(a.CachedInputTokens - b.CachedInputTokens),
		OutputTokens:          maxZero(a.OutputTokens - b.OutputTokens),
		ReasoningOutputTokens: maxZero(a.ReasoningOutputTokens - b.ReasoningOutputTokens),
		TotalTokens:           maxZero(a.TotalTokens - b.TotalTokens),
	}
}

func maxZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

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

func codexTokenCost(u codexTokenUsage) float64 {
	return float64(u.InputTokens)*codexPriceInputPerMTok/1_000_000 +
		float64(u.CachedInputTokens)*codexPriceCachedPerMTok/1_000_000 +
		float64(u.OutputTokens)*codexPriceOutputPerMTok/1_000_000 +
		float64(u.ReasoningOutputTokens)*codexPriceReasoningPerMTok/1_000_000
}

// codexCostMetrics returns cost-today + cost-30d metrics built from the
// local session-log scan. Returns nil if no session data was found for
// the last 30 days so the renderer draws a dash instead of a fake $0.00.
func codexCostMetrics() []providers.MetricValue {
	result, err := scanCodexCosts()
	if err != nil || result == nil {
		return nil
	}
	if result.Today == 0 && result.Last30d == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	today := math.Round(result.Today*100) / 100
	last30 := math.Round(result.Last30d*100) / 100
	return []providers.MetricValue{
		{
			ID:              "cost-today",
			Label:           "TODAY",
			Name:            "Estimated Codex spend today (local logs)",
			Value:           fmt.Sprintf("$%.2f", today),
			NumericValue:    &today,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Cost (local)",
			UpdatedAt:       now,
		},
		{
			ID:              "cost-30d",
			Label:           "30 DAYS",
			Name:            "Estimated Codex spend last 30 days (local logs)",
			Value:           fmt.Sprintf("$%.2f", last30),
			NumericValue:    &last30,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Cost (local)",
			UpdatedAt:       now,
		},
	}
}
