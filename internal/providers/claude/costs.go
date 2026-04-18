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
)

// Cost scan cache — rescan at most once per 5 minutes.
var (
	costMu     sync.Mutex
	costCache  *costResult
	costCacheT time.Time
)

const costCacheTTL = 5 * time.Minute

// Per-million-token pricing (USD). Matches Anthropic's published API rates.
// Cache creation is 1.25x input; cache reads are 0.1x input.
var modelPricing = map[string]struct{ input, output float64 }{
	"claude-opus-4-6":              {15.0, 75.0},
	"claude-opus-4-5-20250414":     {15.0, 75.0},
	"claude-sonnet-4-6":            {3.0, 15.0},
	"claude-sonnet-4-5-20250514":   {3.0, 15.0},
	"claude-sonnet-4-0-20250514":   {3.0, 15.0},
	"claude-haiku-4-5-20251001":    {0.80, 4.0},
	"claude-3-5-sonnet-20241022":   {3.0, 15.0},
	"claude-3-5-haiku-20241022":    {0.80, 4.0},
}

const defaultInputPrice = 3.0  // fallback: Sonnet-class
const defaultOutputPrice = 15.0

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

type costResult struct {
	Today    float64
	Last30d  float64
}

func scanCosts() (*costResult, error) {
	costMu.Lock()
	defer costMu.Unlock()
	if costCache != nil && time.Since(costCacheT) < costCacheTTL {
		return costCache, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &costResult{}, nil
		}
		return nil, err
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	var result costResult

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
			// Quick filter: skip files older than 30 days by mod time
			info, err := f.Info()
			if err != nil || info.ModTime().Before(thirtyDaysAgo) {
				continue
			}
			scanFile(filepath.Join(projPath, f.Name()), todayStart, thirtyDaysAgo, &result)
		}
	}

	costCache = &result
	costCacheT = time.Now()
	return &result, nil
}

func scanFile(path string, todayStart, thirtyDaysAgo time.Time, result *costResult) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

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

func tokenCost(model string, input, output, cacheCreate, cacheRead int) float64 {
	pricing, ok := modelPricing[model]
	if !ok {
		// Try prefix match for versioned model IDs
		for prefix, p := range modelPricing {
			if strings.HasPrefix(model, strings.TrimSuffix(prefix, "-20250414")) {
				pricing = p
				ok = true
				break
			}
		}
		if !ok {
			pricing = struct{ input, output float64 }{defaultInputPrice, defaultOutputPrice}
		}
	}

	inputCost := float64(input) * pricing.input / 1_000_000
	outputCost := float64(output) * pricing.output / 1_000_000
	cacheCreateCost := float64(cacheCreate) * pricing.input * 1.25 / 1_000_000
	cacheReadCost := float64(cacheRead) * pricing.input * 0.10 / 1_000_000

	return inputCost + outputCost + cacheCreateCost + cacheReadCost
}

func costMetrics() []providers.MetricValue {
	result, err := scanCosts()
	if err != nil || result == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var out []providers.MetricValue

	if result.Today > 0 || result.Last30d > 0 {
		today := math.Round(result.Today*100) / 100
		out = append(out, providers.MetricValue{
			ID:              "cost-today",
			Label:           "TODAY",
			Name:            "Estimated spend today",
			Value:           fmt.Sprintf("$%.2f", today),
			NumericValue:    &today,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Cost (local)",
			UpdatedAt:       now,
		})

		last30 := math.Round(result.Last30d*100) / 100
		out = append(out, providers.MetricValue{
			ID:              "cost-30d",
			Label:           "30 DAYS",
			Name:            "Estimated spend last 30 days",
			Value:           fmt.Sprintf("$%.2f", last30),
			NumericValue:    &last30,
			NumericUnit:     "dollars",
			NumericGoodWhen: "low",
			Caption:         "Cost (local)",
			UpdatedAt:       now,
		})
	}

	return out
}
