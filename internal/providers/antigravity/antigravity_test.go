package antigravity

import (
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

func TestParseUserStatusResponse_SelectsCodexBarLanes(t *testing.T) {
	body := []byte(`{
		"code": 0,
		"userStatus": {
			"email": "dev@example.com",
			"userTier": { "name": "Ultra" },
			"cascadeModelConfigData": {
				"clientModelConfigs": [
					{
						"label": "Claude Sonnet 4.5",
						"modelOrAlias": { "model": "claude-sonnet-4-5" },
						"quotaInfo": { "remainingFraction": 0.42, "resetTime": "2030-01-01T00:00:00Z" }
					},
					{
						"label": "Gemini Pro Low",
						"modelOrAlias": { "model": "gemini-2.5-pro-low" },
						"quotaInfo": { "remainingFraction": 0.21 }
					},
					{
						"label": "Gemini Flash",
						"modelOrAlias": { "model": "gemini-2.5-flash" },
						"quotaInfo": { "remainingFraction": 0.84 }
					}
				]
			}
		}
	}`)
	usage, err := parseUserStatusResponse(body)
	if err != nil {
		t.Fatalf("parseUserStatusResponse() error = %v", err)
	}
	if usage.AccountPlan != "Ultra" {
		t.Fatalf("AccountPlan = %q, want Ultra", usage.AccountPlan)
	}
	usage.UpdatedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshot := snapshotFromUsage(usage)
	if len(snapshot.Metrics) != 3 {
		t.Fatalf("metric count = %d, want 3", len(snapshot.Metrics))
	}
	assertMetric(t, snapshot.Metrics[0], "claude-percent", "CLAUDE", 42)
	assertMetric(t, snapshot.Metrics[1], "gemini-pro-percent", "GEMINI PRO", 21)
	assertMetric(t, snapshot.Metrics[2], "gemini-flash-percent", "GEMINI FLASH", 84)
}

func TestExtractFlagHandlesQuotedAndEqualsForms(t *testing.T) {
	command := `language_server_windows.exe --app_data_dir "C:\Users\me\AppData\Roaming\Antigravity" --csrf_token="csrf value" --extension_server_port=12345 --extension_server_csrf_token ext-token`
	if got := extractFlag("--csrf_token", command); got != "csrf value" {
		t.Fatalf("csrf token = %q", got)
	}
	if got := extractFlagInt("--extension_server_port", command); got != 12345 {
		t.Fatalf("extension port = %d", got)
	}
	if got := extractFlag("--extension_server_csrf_token", command); got != "ext-token" {
		t.Fatalf("extension csrf token = %q", got)
	}
}

func TestWindowsListeningPortParsing(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:50123": 50123,
		"0.0.0.0:8080":    8080,
		"[::1]:61234":     61234,
	}
	for input, want := range cases {
		got, ok := portFromAddress(input)
		if !ok || got != want {
			t.Fatalf("portFromAddress(%q) = %d, %v; want %d, true", input, got, ok, want)
		}
	}
}

func assertMetric(t *testing.T, metric providers.MetricValue, id, label string, want float64) {
	t.Helper()
	if metric.ID != id || metric.Label != label {
		t.Fatalf("metric = (%s, %s), want (%s, %s)", metric.ID, metric.Label, id, label)
	}
	if metric.NumericValue == nil || *metric.NumericValue != want {
		t.Fatalf("%s value = %v, want %v", id, metric.NumericValue, want)
	}
}
