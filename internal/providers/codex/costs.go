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
// Each line is an event. We care about events with
//   {"type":"event_msg","payload":{"type":"token_count","info":{...}}}
//
// where info.total_token_usage carries cumulative-since-session-start
// input / cached_input / output / reasoning_output token counts. The
// LAST non-null total_token_usage in the file is the final state of
// that session. We attribute that total to the timestamp of that
// event for day-bucketing.

var (
	codexCostMu      sync.Mutex
	codexCostCache   *codexCostResult
	codexCostCacheT  time.Time
	codexCostCacheErr error
)

const codexCostCacheTTL = 5 * time.Minute

// Per-million-token pricing for GPT-5 class Codex models (USD).
// Codex CLI uses GPT-5 by default (model_context_window 258400 in
// the session logs is the GPT-5 signature). Earlier Codex versions
// used GPT-4.1; rates are similar enough that a single default
// suffices for a "local estimate" metric — we're not invoicing, we
// are giving a gauge.
const (
	codexPriceInputPerMTok    = 1.25 // $ per million input tokens
	codexPriceCachedPerMTok   = 0.125
	codexPriceOutputPerMTok   = 10.0
	codexPriceReasoningPerMTok = 10.0
)

type codexTokenUsage struct {
	InputTokens            int `json:"input_tokens"`
	CachedInputTokens      int `json:"cached_input_tokens"`
	OutputTokens           int `json:"output_tokens"`
	ReasoningOutputTokens  int `json:"reasoning_output_tokens"`
	TotalTokens            int `json:"total_tokens"`
}

type codexTokenEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   *struct {
		Type string `json:"type"`
		Info *struct {
			TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

type codexCostResult struct {
	Today   float64
	Last30d float64
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

	// If the sessions directory doesn't exist (e.g. Codex not installed,
	// or on Windows where the path may differ), return empty — not an
	// error. This keeps the button from entering an error state on
	// platforms where local Codex session logs simply aren't present.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		codexCostCacheErr = nil
		codexCostCache = &codexCostResult{}
		codexCostCacheT = time.Now()
		return codexCostCache, nil
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	var result codexCostResult

	// Walk YYYY/MM/DD/*.jsonl. We only need directories whose date
	// is >= thirtyDaysAgo, but the tree is cheap so a blanket walk
	// is fine.
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable subtrees; don't fail the whole scan.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(thirtyDaysAgo) {
			return nil
		}
		scanCodexSessionFile(path, todayStart, thirtyDaysAgo, &result)
		return nil
	})

	codexCostCacheErr = err
	codexCostCache = &result
	codexCostCacheT = time.Now()
	return codexCostCache, codexCostCacheErr
}

// scanCodexSessionFile walks one rollout JSONL, keeps the LAST non-
// nil total_token_usage seen (which is cumulative for the session),
// then attributes that session's cost to the day of that event.
func scanCodexSessionFile(path string, todayStart, thirtyDaysAgo time.Time, result *codexCostResult) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Codex session lines can be fat (base_instructions carries a
	// multi-paragraph system prompt), so give the buffer room to grow.
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var lastUsage codexTokenUsage
	var lastTS time.Time
	haveUsage := false

	for scanner.Scan() {
		var ev codexTokenEvent
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
		lastUsage = *ev.Payload.Info.TotalTokenUsage
		lastTS = t
		haveUsage = true
	}

	if !haveUsage || lastTS.Before(thirtyDaysAgo) {
		return
	}

	cost := codexTokenCost(lastUsage)
	result.Last30d += cost
	if !lastTS.Before(todayStart) {
		result.Today += cost
	}
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
	// input_tokens in the Codex log is the non-cached chunk; cached
	// is billed at a reduced rate. We also bill reasoning tokens at
	// the output rate.
	return float64(u.InputTokens)*codexPriceInputPerMTok/1_000_000 +
		float64(u.CachedInputTokens)*codexPriceCachedPerMTok/1_000_000 +
		float64(u.OutputTokens)*codexPriceOutputPerMTok/1_000_000 +
		float64(u.ReasoningOutputTokens)*codexPriceReasoningPerMTok/1_000_000
}

// codexCostMetrics returns cost-today + cost-30d metrics built from
// the local session-log scan. Returns nil if no session data was
// found for the last 30 days — the renderer will draw a dash for
// those buttons instead of faking $0.00.
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
			UpdatedAt:       now,
		},
	}
}
