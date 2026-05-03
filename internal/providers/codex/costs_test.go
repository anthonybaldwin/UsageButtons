package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetCodexCostCacheForTest clears the memoized local-cost scan.
func resetCodexCostCacheForTest(t *testing.T) {
	t.Helper()
	codexCostMu.Lock()
	oldCache := codexCostCache
	oldCacheT := codexCostCacheT
	oldCacheErr := codexCostCacheErr
	codexCostCache = nil
	codexCostCacheT = time.Time{}
	codexCostCacheErr = nil
	codexCostMu.Unlock()
	t.Cleanup(func() {
		codexCostMu.Lock()
		codexCostCache = oldCache
		codexCostCacheT = oldCacheT
		codexCostCacheErr = oldCacheErr
		codexCostMu.Unlock()
	})
}

// TestCodexTokenCostUsesModelPricing verifies normalized models use exact pricing.
func TestCodexTokenCostUsesModelPricing(t *testing.T) {
	usage := codexTokenUsage{
		InputTokens:       1_000_000,
		CachedInputTokens: 250_000,
		OutputTokens:      1_000_000,
	}

	base, ok := codexTokenCost("gpt-5", usage)
	if !ok {
		t.Fatal("expected gpt-5 pricing")
	}
	pro, ok := codexTokenCost(normalizeCodexModel("openai/gpt-5.4-pro-2026-04-24"), usage)
	if !ok {
		t.Fatal("expected normalized gpt-5.4-pro pricing")
	}

	if base != 10.96875 {
		t.Fatalf("expected gpt-5 cost 10.96875, got %.5f", base)
	}
	if pro != 210 {
		t.Fatalf("expected normalized gpt-5.4-pro cost 210.00, got %.2f", pro)
	}
}

// TestCodexTokenCostUnknownModelIsSkipped verifies unknown models are unpriced.
func TestCodexTokenCostUnknownModelIsSkipped(t *testing.T) {
	if _, ok := codexTokenCost("unknown-model", codexTokenUsage{InputTokens: 1}); ok {
		t.Fatal("expected unknown model to be skipped")
	}
}

// TestCodexTokenCostFreeModelIsSkipped verifies zero-priced models do not
// mark the session as billable, so a spark-only session hides the widget
// rather than rendering $0.00.
func TestCodexTokenCostFreeModelIsSkipped(t *testing.T) {
	if _, ok := codexTokenCost("gpt-5.3-codex-spark", codexTokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); ok {
		t.Fatal("expected zero-priced model to report ok=false")
	}
}

// TestCodexTokenCostBillsReasoningAsOutput verifies reasoning output uses output pricing.
func TestCodexTokenCostBillsReasoningAsOutput(t *testing.T) {
	got, ok := codexTokenCost("gpt-5", codexTokenUsage{ReasoningOutputTokens: 1_000_000})
	if !ok {
		t.Fatal("expected gpt-5 pricing")
	}
	if got != 10 {
		t.Fatalf("expected reasoning output cost 10.00, got %.2f", got)
	}
}

// TestAdjustInheritedLastDeltaPeelsBaseline verifies inherited cumulative rows are adjusted.
func TestAdjustInheritedLastDeltaPeelsBaseline(t *testing.T) {
	remaining := &codexTokenUsage{InputTokens: 100, CachedInputTokens: 25, OutputTokens: 50}
	got := adjustInheritedLastDelta(
		codexTokenUsage{InputTokens: 130, CachedInputTokens: 25, OutputTokens: 70},
		&remaining,
	)

	if got.InputTokens != 30 || got.CachedInputTokens != 0 || got.OutputTokens != 20 {
		t.Fatalf("unexpected adjusted delta: %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected inherited baseline to be fully consumed, got %+v", *remaining)
	}
}

// TestAdjustInheritedLastDeltaIgnoresOptionalTotalTokens verifies omitted totals do not block baseline peeling.
func TestAdjustInheritedLastDeltaIgnoresOptionalTotalTokens(t *testing.T) {
	remaining := &codexTokenUsage{InputTokens: 100, CachedInputTokens: 25, OutputTokens: 50, TotalTokens: 175}
	got := adjustInheritedLastDelta(
		codexTokenUsage{InputTokens: 130, CachedInputTokens: 25, OutputTokens: 70},
		&remaining,
	)

	if got.InputTokens != 30 || got.CachedInputTokens != 0 || got.OutputTokens != 20 {
		t.Fatalf("unexpected adjusted delta without total_tokens: %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected inherited baseline to be fully consumed, got %+v", *remaining)
	}
}

// TestAdjustInheritedLastDeltaPeelsLargeCumulativeUsage verifies inherited
// rows can be peeled even when every non-zero bucket has grown.
func TestAdjustInheritedLastDeltaPeelsLargeCumulativeUsage(t *testing.T) {
	remaining := &codexTokenUsage{
		InputTokens:          1_000,
		CachedInputTokens:    500,
		CacheReadInputTokens: 500,
		OutputTokens:         250,
	}
	got := adjustInheritedLastDelta(
		codexTokenUsage{
			InputTokens:          1_250,
			CachedInputTokens:    550,
			CacheReadInputTokens: 550,
			OutputTokens:         300,
		},
		&remaining,
	)

	if got.InputTokens != 250 || got.CachedInputTokens != 50 ||
		got.CacheReadInputTokens != 50 || got.OutputTokens != 50 {
		t.Fatalf("unexpected adjusted cumulative delta: %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected inherited baseline to be fully consumed, got %+v", *remaining)
	}
}

// TestAdjustInheritedLastDeltaKeepsLargePerTurnUsage verifies small baselines do not force peeling.
func TestAdjustInheritedLastDeltaKeepsLargePerTurnUsage(t *testing.T) {
	remaining := &codexTokenUsage{InputTokens: 100}
	got := adjustInheritedLastDelta(
		codexTokenUsage{InputTokens: 120},
		&remaining,
	)

	if got.InputTokens != 120 {
		t.Fatalf("expected per-turn usage to stay intact, got %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected per-turn shape to disable inherited peeling, got %+v", *remaining)
	}
}

// TestAdjustInheritedLastDeltaKeepsCachedOnlyPerTurnUsage verifies cached aliases count as one bucket.
func TestAdjustInheritedLastDeltaKeepsCachedOnlyPerTurnUsage(t *testing.T) {
	remaining := &codexTokenUsage{CachedInputTokens: 100, CacheReadInputTokens: 100}
	got := adjustInheritedLastDelta(
		codexTokenUsage{CachedInputTokens: 120, CacheReadInputTokens: 120},
		&remaining,
	)

	if got.CachedInputTokens != 120 || got.CacheReadInputTokens != 120 {
		t.Fatalf("expected cached per-turn usage to stay intact, got %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected per-turn shape to disable inherited peeling, got %+v", *remaining)
	}
}

// TestAdjustInheritedLastDeltaKeepsPerTurnUsage verifies per-turn rows stay intact.
func TestAdjustInheritedLastDeltaKeepsPerTurnUsage(t *testing.T) {
	remaining := &codexTokenUsage{InputTokens: 10_000, CachedInputTokens: 5_000, OutputTokens: 1_000}
	got := adjustInheritedLastDelta(
		codexTokenUsage{InputTokens: 120, CachedInputTokens: 0, OutputTokens: 40},
		&remaining,
	)

	if got.InputTokens != 120 || got.CachedInputTokens != 0 || got.OutputTokens != 40 {
		t.Fatalf("unexpected per-turn delta: %+v", got)
	}
	if remaining != nil {
		t.Fatalf("expected per-turn shape to disable inherited peeling, got %+v", *remaining)
	}
}

// TestReadCodexDeltasBuffersUsageBeforeModelHint verifies early token rows get priced once a model appears.
func TestReadCodexDeltasBuffersUsageBeforeModelHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := `{"timestamp":"2026-04-24T17:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":25,"cached_input_tokens":5,"output_tokens":10}}}}` + "\n" +
		`{"timestamp":"2026-04-24T17:00:01Z","type":"event_msg","payload":{"type":"turn_context","model":"openai/gpt-5.4-pro-2026-04-24"}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	deltas, err := readCodexDeltas(path, nil, codexSessionMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 {
		t.Fatalf("expected one buffered delta, got %d", len(deltas))
	}

	got := deltas[0]
	if got.model != "gpt-5.4-pro" {
		t.Fatalf("expected normalized buffered model, got %q", got.model)
	}
	if got.usage.InputTokens != 25 || got.usage.CachedInputTokens != 5 || got.usage.OutputTokens != 10 {
		t.Fatalf("unexpected buffered usage: %+v", got.usage)
	}
}

// TestReadCodexDeltasReturnsScannerErrors verifies scan failures are surfaced.
func TestReadCodexDeltasReturnsScannerErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 5*1024*1024)), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readCodexDeltas(path, nil, codexSessionMeta{}); err == nil {
		t.Fatal("expected scanner error")
	}
}

// TestReadCodexDeltasFallsBackToZeroBaselineOnMissingParent verifies that an
// unresolved fork parent does not silently drop the child session — it is
// priced from a zero baseline so cost data is not lost when a parent file is
// deleted, permission-blocked, or sat under a walk error.
func TestReadCodexDeltasFallsBackToZeroBaselineOnMissingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := `{"timestamp":"2026-04-24T17:00:00Z","type":"event_msg","payload":{"type":"turn_context","model":"gpt-5"}}` + "\n" +
		`{"timestamp":"2026-04-24T17:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"output_tokens":20}}}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	deltas, err := readCodexDeltas(path, func(string, time.Time) (codexTokenUsage, bool) {
		return codexTokenUsage{}, false
	}, codexSessionMeta{forkedFromID: "missing-parent"})
	if err != nil {
		t.Fatalf("expected zero-baseline fallback, got error: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("expected 1 priced delta from zero baseline, got %d", len(deltas))
	}
	if got := deltas[0].usage.InputTokens; got != 1000 {
		t.Fatalf("expected raw last_token_usage to be priced from zero, got input=%d", got)
	}
}

// TestScanCodexCostsSkipsMalformedSessionFiles verifies one bad file does not blank all local costs.
func TestScanCodexCostsSkipsMalformedSessionFiles(t *testing.T) {
	resetCodexCostCacheForTest(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	root := filepath.Join(sessionsRoot(), "2026", "04", "24")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	good := `{"timestamp":"2026-04-24T17:00:00Z","type":"event_msg","payload":{"type":"turn_context","model":"gpt-5"}}` + "\n" +
		`{"timestamp":"2026-04-24T17:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000000}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "rollout-good.jsonl"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := `{"timestamp":"2026-04-24T17:00:00Z","type":"session_meta","payload":{"session_id":"bad"}}` + "\n" +
		strings.Repeat("x", 5*1024*1024)
	if err := os.WriteFile(filepath.Join(root, "rollout-bad.jsonl"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := scanCodexCosts()
	if err != nil {
		t.Fatalf("expected malformed session to be skipped, got %v", err)
	}
	if result == nil || !result.Native.Seen || result.Native.Last30d <= 0 {
		t.Fatalf("expected valid session to remain priced, got %+v", result)
	}
}

// TestReadCodexSnapshotsSeedsLastTokenUsage verifies snapshot reconstruction includes seed usage.
func TestReadCodexSnapshotsSeedsLastTokenUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := `{"timestamp":"2026-04-24T17:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":25,"cached_input_tokens":5,"output_tokens":10}}}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	snaps, err := readCodexSnapshots(path, codexTokenUsage{InputTokens: 100, CachedInputTokens: 50, OutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(snaps))
	}

	got := snaps[0].usage
	if got.InputTokens != 125 || got.CachedInputTokens != 55 || got.OutputTokens != 30 {
		t.Fatalf("expected seeded last-token snapshot, got %+v", got)
	}
}

// TestReadCodexSnapshotsPeelsInheritedLastTokenUsage verifies cumulative last-token rows do not double count seed usage.
func TestReadCodexSnapshotsPeelsInheritedLastTokenUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := `{"timestamp":"2026-04-24T17:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":125,"cached_input_tokens":50,"output_tokens":30}}}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	snaps, err := readCodexSnapshots(path, codexTokenUsage{InputTokens: 100, CachedInputTokens: 50, OutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(snaps))
	}

	got := snaps[0].usage
	if got.InputTokens != 125 || got.CachedInputTokens != 50 || got.OutputTokens != 30 {
		t.Fatalf("expected inherited last-token snapshot to stay cumulative, got %+v", got)
	}
}
