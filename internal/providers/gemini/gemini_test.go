package gemini

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseQuotaResponseKeepsLowestBucketPerModel(t *testing.T) {
	reset := "2026-04-25T10:30:00Z"
	resp := quotaResponse{Buckets: []quotaBucket{
		{ModelID: "gemini-2.5-pro", RemainingFraction: floatPtr(0.80), ResetTime: "2026-04-25T09:00:00Z"},
		{ModelID: "gemini-2.5-pro", RemainingFraction: floatPtr(0.25), ResetTime: reset},
		{ModelID: "gemini-2.5-flash", RemainingFraction: floatPtr(0.60)},
		{ModelID: "gemini-2.5-flash-lite", RemainingFraction: floatPtr(0.90)},
	}}

	quotas, err := parseQuotaResponse(resp)
	if err != nil {
		t.Fatalf("parseQuotaResponse() error = %v", err)
	}
	if len(quotas) != 3 {
		t.Fatalf("len(quotas) = %d, want 3", len(quotas))
	}
	pro, ok := lowestMatchingQuota(quotas, isProModel)
	if !ok {
		t.Fatal("missing pro quota")
	}
	if pro.PercentLeft != 25 {
		t.Fatalf("pro.PercentLeft = %v, want 25", pro.PercentLeft)
	}
	if pro.ResetTime == nil || pro.ResetTime.Format("2006-01-02T15:04:05Z") != reset {
		t.Fatalf("pro.ResetTime = %v, want %s", pro.ResetTime, reset)
	}
}

func TestSnapshotFromStatusMapsGeminiMetricLanes(t *testing.T) {
	snapshot := snapshotFromStatus(geminiStatus{
		Quotas: []modelQuota{
			{ModelID: "gemini-2.5-flash", PercentLeft: 60},
			{ModelID: "gemini-2.5-flash-lite", PercentLeft: 90},
			{ModelID: "gemini-2.5-pro", PercentLeft: 25},
		},
		Plan: "Paid",
	})

	if snapshot.ProviderName != "Gemini Paid" {
		t.Fatalf("ProviderName = %q, want Gemini Paid", snapshot.ProviderName)
	}
	want := map[string]float64{
		"pro-percent":        25,
		"flash-percent":      60,
		"flash-lite-percent": 90,
	}
	if len(snapshot.Metrics) != len(want) {
		t.Fatalf("len(Metrics) = %d, want %d", len(snapshot.Metrics), len(want))
	}
	for _, metric := range snapshot.Metrics {
		got, ok := want[metric.ID]
		if !ok {
			t.Fatalf("unexpected metric ID %q", metric.ID)
		}
		if metric.NumericValue == nil || *metric.NumericValue != got {
			t.Fatalf("%s NumericValue = %v, want %v", metric.ID, metric.NumericValue, got)
		}
	}
}

func TestPlanLabel(t *testing.T) {
	cases := []struct {
		tier string
		hd   string
		want string
	}{
		{"standard-tier", "", "Paid"},
		{"free-tier", "example.com", "Workspace"},
		{"free-tier", "", "Free"},
		{"legacy-tier", "", "Legacy"},
		{"unknown-tier", "", ""},
	}
	for _, tc := range cases {
		if got := planLabel(tc.tier, tc.hd); got != tc.want {
			t.Fatalf("planLabel(%q, %q) = %q, want %q", tc.tier, tc.hd, got, tc.want)
		}
	}
}

func TestExtractClaimsFromToken(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"email": "dev@example.com",
		"hd":    "example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"

	claims := extractClaimsFromToken(token)
	if claims.Email != "dev@example.com" || claims.HostedDomain != "example.com" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestParseOAuthCredentials(t *testing.T) {
	content := `
const OAUTH_CLIENT_ID = '123456.apps.googleusercontent.com';
const OAUTH_CLIENT_SECRET = "abc-XYZ";
`
	creds, ok := parseOAuthCredentials(content)
	if !ok {
		t.Fatal("parseOAuthCredentials() failed")
	}
	if creds.ClientID != "123456.apps.googleusercontent.com" || creds.ClientSecret != "abc-XYZ" {
		t.Fatalf("creds = %+v", creds)
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
