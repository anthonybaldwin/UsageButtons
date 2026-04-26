package opencodego

import (
	"strings"
	"testing"
	"time"
)

func TestParseSubscription_ActiveUsage(t *testing.T) {
	now := time.Now().UTC()
	text := `{"rollingUsage":{"usagePercent":42,"resetInSec":1800},"weeklyUsage":{"usagePercent":18,"resetInSec":345600},"monthlyUsage":{"usagePercent":7,"resetInSec":2400000}}`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !usage.HasMonthlyUsage {
		t.Error("expected HasMonthlyUsage = true")
	}
	if usage.RollingUsagePercent != 42 || usage.WeeklyUsagePercent != 18 || usage.MonthlyUsagePercent != 7 {
		t.Errorf("percents wrong: rolling=%v weekly=%v monthly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent, usage.MonthlyUsagePercent)
	}
}

func TestParseSubscription_NoSubscription_SolidPage(t *testing.T) {
	now := time.Now().UTC()
	// Solid-rendered /workspace/<id>/go page with no usage numbers —
	// what we expect for a workspace with no Go subscription. Should
	// surface a clear "No active OpenCode Go subscription" error,
	// not a fake zero-usage snapshot.
	text := `<!DOCTYPE html><html><body>
<script>self.$R = self.$R || {};</script>
<div data-server-fn:0="..."></div>
<noscript>OpenCode Go workspace</noscript>
</body></html>`
	_, err := parseSubscription(text, now)
	if err == nil {
		t.Fatal("expected an error for unsubscribed workspace, got nil")
	}
	if !strings.Contains(err.Error(), "No active OpenCode Go subscription") {
		t.Errorf("expected 'No active OpenCode Go subscription' error, got: %v", err)
	}
}

func TestParseSubscription_BrokenResponse_StillErrors(t *testing.T) {
	now := time.Now().UTC()
	// No Solid markers → unknown shape, surface the parse error so
	// genuine schema regressions stay visible.
	text := `<html><body><h1>500 Internal Server Error</h1></body></html>`
	_, err := parseSubscription(text, now)
	if err == nil {
		t.Fatal("expected error for unrecognized response, got nil")
	}
	if !strings.Contains(err.Error(), "missing usage fields") {
		t.Errorf("expected 'missing usage fields' error, got: %v", err)
	}
}
