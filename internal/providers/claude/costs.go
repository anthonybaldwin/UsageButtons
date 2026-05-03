package claude

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

// Cost scan cache — rescan at most once per 5 minutes.
var (
	// costMu guards costCache and costCacheT.
	costMu sync.Mutex
	// costCache holds the most recent result from scanCosts (covering
	// both the Windows-native projects dir and any running WSL distros).
	costCache *allCostResults
	// costCacheT is the wall-clock time of the last successful scan.
	costCacheT time.Time
	// unpricedModelMu guards unpricedModelCounts.
	unpricedModelMu sync.Mutex
	// unpricedModelCounts tracks skipped token totals by normalized model ID.
	unpricedModelCounts = make(map[string]int)
)

// costCacheTTL bounds how often scanCosts re-reads the session JSONL files.
const costCacheTTL = 5 * time.Minute

// claudePricing stores per-million-token prices in USD.
type claudePricing struct {
	// input is the per-million input token price in USD.
	input float64
	// output is the per-million output token price in USD.
	output float64
	// cacheCreate is the per-million cache-write token price in USD.
	cacheCreate float64
	// cacheRead is the per-million cache-read token price in USD.
	cacheRead float64
	// threshold is the input-side token count above which long-context rates apply.
	threshold int
	// inputAbove optionally overrides input pricing when the request crosses threshold.
	inputAbove *float64
	// outputAbove optionally overrides output pricing when the request crosses threshold.
	outputAbove *float64
	// cacheCreateAbove optionally overrides cache-write pricing when the request crosses threshold.
	cacheCreateAbove *float64
	// cacheReadAbove optionally overrides cache-read pricing when the request crosses threshold.
	cacheReadAbove *float64
}

// modelPricing mirrors CodexBar's Claude pricing table for local CLI logs.
var modelPricing = map[string]claudePricing{
	"claude-haiku-4-5-20251001": {input: 1.0, output: 5.0, cacheCreate: 1.25, cacheRead: 0.1},
	"claude-haiku-4-5":          {input: 1.0, output: 5.0, cacheCreate: 1.25, cacheRead: 0.1},
	"claude-opus-4-5-20251101":  {input: 5.0, output: 25.0, cacheCreate: 6.25, cacheRead: 0.5},
	"claude-opus-4-5":           {input: 5.0, output: 25.0, cacheCreate: 6.25, cacheRead: 0.5},
	"claude-opus-4-6-20260205":  {input: 5.0, output: 25.0, cacheCreate: 6.25, cacheRead: 0.5},
	"claude-opus-4-6":           {input: 5.0, output: 25.0, cacheCreate: 6.25, cacheRead: 0.5},
	"claude-opus-4-7":           {input: 5.0, output: 25.0, cacheCreate: 6.25, cacheRead: 0.5},
	"claude-sonnet-4-5": {
		input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3, threshold: 200_000,
		inputAbove: ptrFloat(6.0), outputAbove: ptrFloat(22.5), cacheCreateAbove: ptrFloat(7.5), cacheReadAbove: ptrFloat(0.6),
	},
	"claude-sonnet-4-6": {
		input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3,
	},
	"claude-sonnet-4-5-20250929": {
		input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3, threshold: 200_000,
		inputAbove: ptrFloat(6.0), outputAbove: ptrFloat(22.5), cacheCreateAbove: ptrFloat(7.5), cacheReadAbove: ptrFloat(0.6),
	},
	"claude-opus-4-20250514": {input: 15.0, output: 75.0, cacheCreate: 18.75, cacheRead: 1.5},
	"claude-opus-4-1":        {input: 15.0, output: 75.0, cacheCreate: 18.75, cacheRead: 1.5},
	"claude-sonnet-4-20250514": {
		input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3, threshold: 200_000,
		inputAbove: ptrFloat(6.0), outputAbove: ptrFloat(22.5), cacheCreateAbove: ptrFloat(7.5), cacheReadAbove: ptrFloat(0.6),
	},
	// Legacy Claude 3.5 models still appear in older session logs.
	// normalizeClaudeModel strips dated suffixes when the base is priced,
	// so these bases cover both undated and -YYYYMMDD variants.
	"claude-3-5-sonnet": {input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3},
	"claude-3-5-haiku":  {input: 0.80, output: 4.0, cacheCreate: 1.0, cacheRead: 0.08},
	// claude-sonnet-4-0 is the pre-rename form of Sonnet 4 that still
	// shows up as claude-sonnet-4-0-YYYYMMDD in older Vertex/Bedrock logs.
	"claude-sonnet-4-0": {
		input: 3.0, output: 15.0, cacheCreate: 3.75, cacheRead: 0.3, threshold: 200_000,
		inputAbove: ptrFloat(6.0), outputAbove: ptrFloat(22.5), cacheCreateAbove: ptrFloat(7.5), cacheReadAbove: ptrFloat(0.6),
	},
}

// sessionRecord is the shape of one line in a Claude session .jsonl file.
type sessionRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// costResult aggregates the estimated spend across session files.
type costResult struct {
	// Today is the estimated spend since local midnight.
	Today float64
	// Last30d is the estimated spend over the trailing 30 days.
	Last30d float64
}

// allCostResults holds the costResult for the Windows-native projects
// dir plus, on Windows builds with WSL installed and distros running,
// one costResult per running distro keyed by Source.Key.
//
// Each distro is treated as its own "machine" — no aggregation with
// the native scope, since the user explicitly wants them visible
// separately rather than silently summed.
type allCostResults struct {
	Native costResult
	// WSL is keyed by wsl.Source.Key. Empty/nil when WSL is unavailable
	// or no distros are running.
	WSL map[string]costResult
	// WSLLabels maps Source.Key → friendly distro name (e.g.
	// "Ubuntu-22.04") so the metric Caption can identify each scope
	// without re-running discovery.
	WSLLabels map[string]string
}

// scanCosts walks ~/.claude/projects on the Windows host plus the
// equivalent path inside every running WSL distro, returning the
// per-scope aggregates memoized for costCacheTTL.
func scanCosts() (*allCostResults, error) {
	costMu.Lock()
	defer costMu.Unlock()
	if costCache != nil && time.Since(costCacheT) < costCacheTTL {
		return costCache, nil
	}
	resetUnpricedModels()

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	out := &allCostResults{}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	native, err := scanProjectsDir(filepath.Join(home, ".claude", "projects"), todayStart, thirtyDaysAgo)
	if err != nil {
		return nil, err
	}
	out.Native = native

	// wsl.Sources() is a no-op on non-Windows builds and returns nil
	// when WSL isn't installed or no distros are running, so this loop
	// degrades cleanly to zero extra work in the common case.
	if sources := wsl.Sources(); len(sources) > 0 {
		out.WSL = make(map[string]costResult, len(sources))
		out.WSLLabels = make(map[string]string, len(sources))
		for _, src := range sources {
			r, err := scanProjectsDir(filepath.Join(src.Home, ".claude", "projects"), todayStart, thirtyDaysAgo)
			if err != nil {
				// One unreachable distro shouldn't poison the whole
				// scan; just skip it and keep going.
				continue
			}
			out.WSL[src.Key] = r
			out.WSLLabels[src.Key] = src.Label
		}
	}

	costCache = out
	costCacheT = time.Now()
	return out, nil
}

// scanProjectsDir walks one Claude projects directory (on any
// filesystem — native or \\wsl.localhost\...) and returns the
// today/30d cost aggregate. Missing directories return a zero result
// without error so callers can scan optional scopes safely.
func scanProjectsDir(projectsDir string, todayStart, thirtyDaysAgo time.Time) (costResult, error) {
	var result costResult
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}

	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, project.Name())
		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			// Quick filter: skip files older than 30 days by mod time.
			info, err := f.Info()
			if err != nil || info.ModTime().Before(thirtyDaysAgo) {
				continue
			}
			scanFile(filepath.Join(projPath, f.Name()), todayStart, thirtyDaysAgo, &result)
		}
	}
	return result, nil
}

// scanFile parses one session .jsonl file and accumulates token costs
// into result for entries newer than thirtyDaysAgo.
func scanFile(path string, todayStart, thirtyDaysAgo time.Time, result *costResult) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Use a 10MB buffer to handle large prompt/response lines in local logs.
	const maxLine = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 256*1024), maxLine)

	for scanner.Scan() {
		var rec sessionRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message == nil || rec.Message.Usage == nil {
			continue
		}

		t, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil {
			t, err = time.Parse(time.RFC3339, rec.Timestamp)
			if err != nil {
				continue
			}
		}

		if t.Before(thirtyDaysAgo) {
			continue
		}

		cost := tokenCost(rec.Message.Model, rec.Message.Usage.InputTokens, rec.Message.Usage.OutputTokens,
			rec.Message.Usage.CacheCreationInputTokens, rec.Message.Usage.CacheReadInputTokens)

		result.Last30d += cost
		if !t.Before(todayStart) {
			result.Today += cost
		}
	}
}

// tokenCost returns the estimated USD cost for a single assistant message
// given its model and per-bucket token counts.
func tokenCost(model string, input, output, cacheCreate, cacheRead int) float64 {
	normalized := normalizeClaudeModel(model)
	pricing, ok := modelPricing[normalized]
	if !ok {
		recordUnpricedModel(model, normalized, input, output, cacheCreate, cacheRead)
		return 0
	}

	totalInputSide := maxZero(input) + maxZero(cacheCreate) + maxZero(cacheRead)
	longContext := pricing.threshold > 0 && totalInputSide > pricing.threshold
	inputCost := tokenBucketCost(input, pricing.input, pricing.inputAbove, longContext)
	outputCost := tokenBucketCost(output, pricing.output, pricing.outputAbove, longContext)
	cacheCreateCost := tokenBucketCost(cacheCreate, pricing.cacheCreate, pricing.cacheCreateAbove, longContext)
	cacheReadCost := tokenBucketCost(cacheRead, pricing.cacheRead, pricing.cacheReadAbove, longContext)

	return inputCost + outputCost + cacheCreateCost + cacheReadCost
}

// recordUnpricedModel tracks model IDs whose local pricing is unknown.
func recordUnpricedModel(model, normalized string, input, output, cacheCreate, cacheRead int) {
	if normalized == "" {
		normalized = strings.TrimSpace(model)
	}
	tokens := maxZero(input) + maxZero(output) + maxZero(cacheCreate) + maxZero(cacheRead)

	unpricedModelMu.Lock()
	unpricedModelCounts[normalized] += tokens
	total := unpricedModelCounts[normalized]
	unpricedModelMu.Unlock()

	if total == tokens && tokens > 0 {
		logf("unknown local cost model %q normalized as %q; skipping %d tokens", model, normalized, tokens)
	}
}

// resetUnpricedModels clears per-scan observability for unknown local pricing.
func resetUnpricedModels() {
	unpricedModelMu.Lock()
	unpricedModelCounts = make(map[string]int)
	unpricedModelMu.Unlock()
}

// tokenBucketCost applies the selected per-million-token rate to a bucket.
func tokenBucketCost(tokens int, base float64, above *float64, longContext bool) float64 {
	rate := base
	if longContext && above != nil {
		rate = *above
	}
	return float64(maxZero(tokens)) * rate / 1_000_000
}

// normalizeClaudeModel maps provider-prefixed and versioned Claude model IDs
// onto pricing-table keys.
func normalizeClaudeModel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	// Remove the common Anthropic provider prefix before table lookup.
	trimmed = strings.TrimPrefix(trimmed, "anthropic.")
	// Some provider IDs are dot-qualified; keep the tail when it is a Claude ID.
	if lastDot := strings.LastIndex(trimmed, "."); lastDot >= 0 && strings.Contains(trimmed, "claude-") {
		tail := trimmed[lastDot+1:]
		if strings.HasPrefix(tail, "claude-") {
			trimmed = tail
		}
	}
	// Strip Bedrock-style -vN:M suffixes after verifying the suffix is numeric.
	if idx := strings.Index(trimmed, "-v"); idx >= 0 {
		suffix := trimmed[idx+2:]
		if colon := strings.IndexByte(suffix, ':'); colon > 0 {
			allDigits := true
			for _, r := range suffix[:colon] + suffix[colon+1:] {
				if r < '0' || r > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				trimmed = trimmed[:idx]
			}
		}
	}
	if _, ok := modelPricing[trimmed]; ok {
		return trimmed
	}
	// Strip Vertex-style @YYYYMMDD date tags when the base model is priced.
	if at := strings.LastIndexByte(trimmed, '@'); at > 0 {
		if _, err := time.Parse("20060102", trimmed[at+1:]); err == nil {
			base := trimmed[:at]
			if _, ok := modelPricing[base]; ok {
				return base
			}
		}
	}
	// Strip trailing -YYYYMMDD date tags when the base model is priced.
	if len(trimmed) > 9 {
		base := trimmed[:len(trimmed)-9]
		suffix := trimmed[len(trimmed)-9:]
		if strings.HasPrefix(suffix, "-") {
			if _, err := time.Parse("20060102", suffix[1:]); err == nil {
				if _, ok := modelPricing[base]; ok {
					return base
				}
			}
		}
	}
	return trimmed
}

// maxZero clamps n to a non-negative int.
func maxZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// ptrFloat returns a pointer to v.
func ptrFloat(v float64) *float64 {
	return &v
}

// costMetrics renders the scanned spend into MetricValue tiles for the
// trailing day and 30 days. The Windows-native scope produces the
// stable "cost-today" / "cost-30d" IDs; each running WSL distro
// produces a parallel pair suffixed with the distro key
// (e.g. "cost-today-wsl-Debian"), treated as a separate machine.
func costMetrics() []providers.MetricValue {
	result, err := scanCosts()
	if err != nil || result == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var out []providers.MetricValue

	emit := func(scopeSuffix, captionSuffix string, r costResult) {
		// Skip empty scopes so a fresh WSL distro with no sessions
		// doesn't render a fake $0.00 tile.
		if r.Today == 0 && r.Last30d == 0 {
			return
		}
		today := math.Round(r.Today*100) / 100
		last30 := math.Round(r.Last30d*100) / 100
		out = append(out,
			providers.MetricValue{
				ID:              "cost-today" + scopeSuffix,
				Label:           "TODAY",
				Name:            "Estimated spend today" + captionSuffix,
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
				Name:            "Estimated spend last 30 days" + captionSuffix,
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

	return out
}
