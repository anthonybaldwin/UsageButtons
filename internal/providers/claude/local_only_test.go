package claude

import "testing"

func TestActiveIsLocalOnly_NilFallsThrough(t *testing.T) {
	if claudeActiveIsLocalOnly(nil) {
		t.Error("nil active set must NOT short-circuit — that's the cold-start path")
	}
}

func TestActiveIsLocalOnly_EmptyFallsThrough(t *testing.T) {
	if claudeActiveIsLocalOnly([]string{}) {
		t.Error("empty active set must NOT short-circuit")
	}
}

func TestActiveIsLocalOnly_OnlyCostMetricsShortCircuit(t *testing.T) {
	cases := [][]string{
		{"cost-today"},
		{"cost-30d"},
		{"cost-today", "cost-30d"},
	}
	for _, c := range cases {
		if !claudeActiveIsLocalOnly(c) {
			t.Errorf("%v: expected short-circuit (every entry is local)", c)
		}
	}
}

func TestActiveIsLocalOnly_AnyLiveMetricBlocks(t *testing.T) {
	cases := [][]string{
		{"session-percent"},
		{"weekly-percent", "cost-today"},
		{"cost-today", "extra-usage-balance"},
	}
	for _, c := range cases {
		if claudeActiveIsLocalOnly(c) {
			t.Errorf("%v: must not short-circuit — at least one entry needs the API", c)
		}
	}
}
