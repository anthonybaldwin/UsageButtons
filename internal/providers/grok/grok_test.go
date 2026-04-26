package grok

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

func mustParse(t *testing.T, body string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("test fixture invalid: %v", err)
	}
	return out
}

func TestParseModelStats_Grok3FullShape(t *testing.T) {
	// Shape captured from a real grok-3 rate-limits response (per
	// JoshuaWang2211/grok-usage-watch's inspection of the endpoint).
	root := mustParse(t, `{
		"remainingQueries": 25,
		"totalQueries": 50,
		"remainingTokens": 18000,
		"totalTokens": 25000,
		"windowSizeSeconds": 7200,
		"lowEffortRateLimits": {"remainingQueries": 25, "cost": 1, "waitTimeSeconds": 0},
		"highEffortRateLimits": {"remainingQueries": 5, "cost": 10, "waitTimeSeconds": 0}
	}`)
	stats := parseModelStats(root)
	if stats.RemainingQueries == nil || *stats.RemainingQueries != 25 {
		t.Errorf("RemainingQueries: got %v, want 25", stats.RemainingQueries)
	}
	if stats.TotalQueries == nil || *stats.TotalQueries != 50 {
		t.Errorf("TotalQueries: got %v, want 50", stats.TotalQueries)
	}
	if stats.RemainingTokens == nil || *stats.RemainingTokens != 18000 {
		t.Errorf("RemainingTokens: got %v, want 18000", stats.RemainingTokens)
	}
	if stats.TotalTokens == nil || *stats.TotalTokens != 25000 {
		t.Errorf("TotalTokens: got %v, want 25000", stats.TotalTokens)
	}
	if stats.WindowSizeSeconds == nil || *stats.WindowSizeSeconds != 7200 {
		t.Errorf("WindowSizeSeconds: got %v, want 7200", stats.WindowSizeSeconds)
	}
}

func TestParseModelStats_Grok4HeavyMinimalShape(t *testing.T) {
	// grok-4-heavy returns a smaller subset (just queries, no tokens
	// per the upstream tracker's analysis).
	root := mustParse(t, `{"remainingQueries": 3, "totalQueries": 20, "windowSizeSeconds": 3600}`)
	stats := parseModelStats(root)
	if stats.RemainingQueries == nil || *stats.RemainingQueries != 3 {
		t.Errorf("RemainingQueries: got %v", stats.RemainingQueries)
	}
	if stats.TotalQueries == nil || *stats.TotalQueries != 20 {
		t.Errorf("TotalQueries: got %v", stats.TotalQueries)
	}
	if stats.RemainingTokens != nil {
		t.Errorf("RemainingTokens should be nil for tokenless response, got %v", *stats.RemainingTokens)
	}
}

func TestParseModelStats_SnakeCaseFallback(t *testing.T) {
	// Defensive: API rename to snake_case keeps working.
	root := mustParse(t, `{"remaining_queries": 7, "total_queries": 10, "window_size_seconds": 60}`)
	stats := parseModelStats(root)
	if stats.RemainingQueries == nil || *stats.RemainingQueries != 7 {
		t.Errorf("RemainingQueries snake_case: got %v", stats.RemainingQueries)
	}
	if stats.TotalQueries == nil || *stats.TotalQueries != 10 {
		t.Errorf("TotalQueries snake_case: got %v", stats.TotalQueries)
	}
}

func TestParseModelStats_EmptyResponse(t *testing.T) {
	stats := parseModelStats(mustParse(t, `{}`))
	if stats.RemainingQueries != nil {
		t.Errorf("expected nil for missing keys, got %v", *stats.RemainingQueries)
	}
}

func TestCountMetric_SkipsWhenTotalAbsent(t *testing.T) {
	r := 5
	if _, ok := countMetric("x", "X", "Cat", "X", &r, nil, nil, ""); ok {
		t.Error("expected countMetric to skip when total is nil")
	}
}

func TestCountMetric_SkipsWhenZeroTotal(t *testing.T) {
	r := 5
	z := 0
	if _, ok := countMetric("x", "X", "Cat", "X", &r, &z, nil, ""); ok {
		t.Error("expected countMetric to skip when total <= 0")
	}
}

func TestCountMetric_BuildsForValidShape(t *testing.T) {
	r := 30
	tot := 50
	m, ok := countMetric("grok3-queries-remaining", "GROK 3", "Queries", "Grok 3 queries", &r, &tot, nil, "")
	if !ok {
		t.Fatal("expected metric to be produced")
	}
	if m.ID != "grok3-queries-remaining" {
		t.Errorf("ID: got %q", m.ID)
	}
	if v, ok := m.Value.(string); !ok || v != "30/50" {
		t.Errorf("Value: got %v (%T), want \"30/50\" string", m.Value, m.Value)
	}
	if m.NumericUnit != "count" {
		t.Errorf("NumericUnit: got %q, want count", m.NumericUnit)
	}
	// Ratio intentionally NOT set — Grok renders as a reference
	// card (no meter fill bar). The count itself is the focal text.
	if m.Ratio != nil {
		t.Errorf("Ratio should be nil (reference card), got %v", *m.Ratio)
	}
	// RawCount/RawMax intentionally NOT set — the button's Value is
	// already "X/Y" so surfacing the same fraction via the rawCounts
	// override would clobber the category caption.
	if m.RawCount != nil {
		t.Errorf("RawCount should be nil so caption isn't overridden, got %v", *m.RawCount)
	}
	if m.RawMax != nil {
		t.Errorf("RawMax should be nil so caption isn't overridden, got %v", *m.RawMax)
	}
	if m.ResetInSeconds != nil {
		t.Errorf("ResetInSeconds should be nil while remaining > 0, got %v", *m.ResetInSeconds)
	}
	if m.Caption != "Queries" {
		t.Errorf("Caption: got %q, want %q", m.Caption, "Queries")
	}
}

func TestCountMetric_SetsCountdownOnlyWhenRateLimited(t *testing.T) {
	zero := 0
	tot := 50
	wait := 540
	m, ok := countMetric("x", "X", "Cat", "X", &zero, &tot, &wait, "")
	if !ok {
		t.Fatal("metric should still build with remaining=0")
	}
	if m.ResetInSeconds == nil {
		t.Fatal("ResetInSeconds should be set when remaining=0 and waitSecs>0")
	}
	if *m.ResetInSeconds != 540 {
		t.Errorf("ResetInSeconds: got %v, want 540", *m.ResetInSeconds)
	}
}

func TestCountMetric_NoCountdownWhileQuotaRemains(t *testing.T) {
	r := 5
	tot := 50
	wait := 999 // present but should be ignored while r > 0
	m, _ := countMetric("x", "X", "Cat", "X", &r, &tot, &wait, "")
	if m.ResetInSeconds != nil {
		t.Errorf("ResetInSeconds should be nil while remaining=%d > 0, got %v", r, *m.ResetInSeconds)
	}
}

func TestReadWaitTimeSeconds_PrefersSmaller(t *testing.T) {
	root := mustParse(t, `{
		"remainingQueries":0, "totalQueries":50,
		"lowEffortRateLimits": {"waitTimeSeconds": 540},
		"highEffortRateLimits": {"waitTimeSeconds": 1800}
	}`)
	got := readWaitTimeSeconds(root)
	if got == nil || *got != 540 {
		t.Errorf("expected smallest wait (540s), got %v", got)
	}
}

func TestReadWaitTimeSeconds_NilOnHealthyResponse(t *testing.T) {
	root := mustParse(t, `{"lowEffortRateLimits": null, "highEffortRateLimits": null}`)
	if got := readWaitTimeSeconds(root); got != nil {
		t.Errorf("expected nil on healthy response, got %v", *got)
	}
}

func TestSnapshotFromUsage_Grok3OnlyOmitsHeavy(t *testing.T) {
	// Free tier: grok-4-heavy returns no data; only grok-3 metrics emit.
	r3, t3 := 25, 50
	rT, tT := 18000, 25000
	w := 7200
	usage := usageSnapshot{
		Grok3: modelStats{
			RemainingQueries: &r3, TotalQueries: &t3,
			RemainingTokens: &rT, TotalTokens: &tT,
			WindowSizeSeconds: &w,
		},
		UpdatedAt: time.Now().UTC(),
	}
	snap := snapshotFromUsage(usage)
	if len(snap.Metrics) != 2 {
		t.Fatalf("expected 2 metrics (grok3 queries + tokens), got %d", len(snap.Metrics))
	}
	got := []string{snap.Metrics[0].ID, snap.Metrics[1].ID}
	want := []string{"grok3-queries-remaining", "grok3-tokens-remaining"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("metric[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSnapshotFromUsage_AllThreeWhenHeavyPresent(t *testing.T) {
	r3, t3 := 25, 50
	rT, tT := 18000, 25000
	r4, t4 := 3, 20
	w := 3600
	usage := usageSnapshot{
		Grok3: modelStats{RemainingQueries: &r3, TotalQueries: &t3, RemainingTokens: &rT, TotalTokens: &tT, WindowSizeSeconds: &w},
		Grok4: modelStats{RemainingQueries: &r4, TotalQueries: &t4, WindowSizeSeconds: &w},
		UpdatedAt: time.Now().UTC(),
	}
	snap := snapshotFromUsage(usage)
	if len(snap.Metrics) != 3 {
		t.Fatalf("expected 3 metrics, got %d", len(snap.Metrics))
	}
}

func TestMapHTTPError_Stale401(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{Status: 401})
	if snap.Status != "unknown" || snap.Error == "" {
		t.Errorf("expected stale message, got %+v", snap)
	}
}

func TestMapHTTPError_GenericNonHTTP500NoBodyLeak(t *testing.T) {
	snap := mapHTTPError(&httputil.Error{
		Status: 500,
		Body:   `<html>internal error with secret=abc</html>`,
		URL:    "https://grok.com/rest/rate-limits",
	})
	if strings.Contains(snap.Error, "secret=abc") {
		t.Errorf("body leaked into user-visible error: %q", snap.Error)
	}
	if !strings.Contains(snap.Error, "HTTP 500") {
		t.Errorf("expected short HTTP code, got %q", snap.Error)
	}
}

func TestMapHTTPError_NetworkError(t *testing.T) {
	snap := mapHTTPError(errors.New("dial tcp: timeout"))
	if !strings.Contains(snap.Error, "dial tcp") {
		t.Errorf("expected raw network error preserved, got %q", snap.Error)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := Provider{}
	if p.ID() != "grok" {
		t.Errorf("ID: got %q", p.ID())
	}
	if p.Name() != "Grok" {
		t.Errorf("Name: got %q", p.Name())
	}
	if p.BrandColor() == "" || p.BrandBg() == "" {
		t.Error("BrandColor/BrandBg should not be empty")
	}
	want := map[string]bool{
		"grok3-queries-remaining":       true,
		"grok3-tokens-remaining":        true,
		"grok4-heavy-queries-remaining": true,
	}
	for _, id := range p.MetricIDs() {
		if !want[id] {
			t.Errorf("unexpected metric ID %q", id)
		}
	}
}
