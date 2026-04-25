package vertexai

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMatchedQuotaPercents(t *testing.T) {
	usage := []monitoringTimeSeries{
		series("aiplatform.googleapis.com/generate_content_requests", "requests_per_minute", "us-central1", 75),
		series("aiplatform.googleapis.com/input_tokens", "tokens_per_minute", "us-central1", 400),
	}
	limits := []monitoringTimeSeries{
		series("aiplatform.googleapis.com/generate_content_requests", "requests_per_minute", "us-central1", 100),
		series("aiplatform.googleapis.com/input_tokens", "tokens_per_minute", "us-central1", 1000),
	}

	percents, err := matchedQuotaPercents(usage, limits)
	if err != nil {
		t.Fatalf("matchedQuotaPercents() error = %v", err)
	}
	if len(percents) != 2 {
		t.Fatalf("len(percents) = %d, want 2", len(percents))
	}
	requests := maxQuotaPercent(percents, isRequestQuota)
	if requests == nil || requests.UsedPct != 75 {
		t.Fatalf("request percent = %+v, want 75", requests)
	}
	tokens := maxQuotaPercent(percents, isTokenQuota)
	if tokens == nil || tokens.UsedPct != 40 {
		t.Fatalf("token percent = %+v, want 40", tokens)
	}
}

func TestSnapshotFromUsage(t *testing.T) {
	tokens := 40.0
	snapshot := snapshotFromUsage(quotaUsage{
		RequestsUsedPercent: 75,
		TokensUsedPercent:   &tokens,
		ProjectID:           "demo-project",
	})

	if len(snapshot.Metrics) != 2 {
		t.Fatalf("len(Metrics) = %d, want 2", len(snapshot.Metrics))
	}
	want := map[string]float64{
		"session-percent": 25,
		"weekly-percent":  60,
	}
	for _, metric := range snapshot.Metrics {
		got, ok := want[metric.ID]
		if !ok {
			t.Fatalf("unexpected metric %q", metric.ID)
		}
		if metric.NumericValue == nil || *metric.NumericValue != got {
			t.Fatalf("%s NumericValue = %v, want %v", metric.ID, metric.NumericValue, got)
		}
	}
}

func TestLoadProjectIDFromGcloudConfig(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "configurations")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, defaultConfigFile), []byte("[core]\nproject = demo-project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDSDK_CONFIG", dir)

	if got := loadProjectID(); got != "demo-project" {
		t.Fatalf("loadProjectID() = %q, want demo-project", got)
	}
}

func TestEmailFromIDToken(t *testing.T) {
	payload, err := json.Marshal(map[string]string{"email": "dev@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"

	if got := emailFromIDToken(token); got != "dev@example.com" {
		t.Fatalf("emailFromIDToken() = %q, want dev@example.com", got)
	}
}

func series(quotaMetric, limitName, location string, value float64) monitoringTimeSeries {
	return monitoringTimeSeries{
		Metric: monitoringMetric{Labels: map[string]string{
			"quota_metric": quotaMetric,
			"limit_name":   limitName,
		}},
		Resource: monitoringResource{Labels: map[string]string{
			"location": location,
		}},
		Points: []monitoringPoint{{Value: monitoringValue{DoubleValue: &value}}},
	}
}
