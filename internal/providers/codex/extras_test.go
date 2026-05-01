package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestParseCreditEvents_Array covers the bare-array shape — newest by
// timestamp wins.
func TestParseCreditEvents_Array(t *testing.T) {
	raw := json.RawMessage(`[
		{"timestamp":"2026-04-30T10:00:00Z","credits_used":1.23,"service":"old"},
		{"timestamp":"2026-04-30T11:00:00Z","credits_used":4.56,"service":"new"}
	]`)
	ev := parseCreditEvents(raw)
	if ev == nil {
		t.Fatal("expected an event")
	}
	if ev.Service != "new" {
		t.Fatalf("expected newest by timestamp, got %q", ev.Service)
	}
}

// TestParseCreditEvents_ObjectWrappings covers the events / items / data
// wrappers chatgpt.com has shipped at various times.
func TestParseCreditEvents_ObjectWrappings(t *testing.T) {
	cases := map[string]string{
		"events": `{"events":[{"timestamp":"2026-04-30T10:00:00Z","credits_used":1.0,"service":"e"}]}`,
		"items":  `{"items":[{"timestamp":"2026-04-30T10:00:00Z","credits_used":1.0,"service":"i"}]}`,
		"data":   `{"data":[{"timestamp":"2026-04-30T10:00:00Z","credits_used":1.0,"service":"d"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ev := parseCreditEvents(json.RawMessage(body))
			if ev == nil {
				t.Fatal("expected an event")
			}
			if ev.Service != string(name[0]) {
				t.Fatalf("got service %q", ev.Service)
			}
		})
	}
}

// TestParseCreditEvents_UnknownShape verifies a payload we can't make
// sense of returns nil so the caller can log + skip the metric.
func TestParseCreditEvents_UnknownShape(t *testing.T) {
	if ev := parseCreditEvents(json.RawMessage(`{"weird":true}`)); ev != nil {
		t.Fatalf("expected nil for unknown shape, got %+v", ev)
	}
}

// TestPickNewestEvent_FallsBackToFirstWhenNoTimestamps ensures we still
// surface a metric when none of the rows carry a parseable timestamp;
// the API ships newest-first, so [0] is the best guess.
func TestPickNewestEvent_FallsBackToFirstWhenNoTimestamps(t *testing.T) {
	events := []creditEvent{
		{Service: "first", CreditsUsed: 1},
		{Service: "second", CreditsUsed: 2},
	}
	ev := pickNewestEvent(events)
	if ev == nil || ev.Service != "first" {
		t.Fatalf("expected first row fallback, got %+v", ev)
	}
}

// TestParseEventTS_VariantsAndLayouts checks each timestamp field name
// and both RFC3339 + RFC3339Nano layouts.
func TestParseEventTS_VariantsAndLayouts(t *testing.T) {
	cases := []creditEvent{
		{Timestamp: "2026-04-30T10:00:00Z"},
		{CreatedAt: "2026-04-30T10:00:00.5Z"},
		{CreatedAtC: "2026-04-30T10:00:00Z"},
		{OccurredAt: "2026-04-30T10:00:00Z"},
		{OccurredAtC: "2026-04-30T10:00:00Z"},
	}
	for i, ev := range cases {
		if _, ok := parseEventTS(ev); !ok {
			t.Fatalf("case %d: expected parseable timestamp", i)
		}
	}
	if _, ok := parseEventTS(creditEvent{Timestamp: "garbage"}); ok {
		t.Fatal("expected unparseable timestamp to fail")
	}
}

// TestEventAmount_VariantPrecedence exercises the field-name precedence
// fetchLastCreditEvent relies on for backwards-compatibility with old
// API renames.
func TestEventAmount_VariantPrecedence(t *testing.T) {
	if got := eventAmount(creditEvent{CreditsUsed: 1.5, CreditsC: 9}); got != 1.5 {
		t.Fatalf("credits_used should win, got %v", got)
	}
	if got := eventAmount(creditEvent{CreditsC: 2.0, Credits: 9}); got != 2.0 {
		t.Fatalf("creditsUsed should win when credits_used is zero, got %v", got)
	}
	if got := eventAmount(creditEvent{Amount: 3.0}); got != 3.0 {
		t.Fatalf("amount fallback failed, got %v", got)
	}
	if got := eventAmount(creditEvent{}); got != 0 {
		t.Fatalf("empty event should be zero, got %v", got)
	}
}

// TestEventService_VariantPrecedence mirrors eventAmount but for the
// service-label fields.
func TestEventService_VariantPrecedence(t *testing.T) {
	if got := eventService(creditEvent{ServiceName: "a", ServiceC: "b"}); got != "a" {
		t.Fatalf("service_name should win, got %q", got)
	}
	if got := eventService(creditEvent{ServiceC: "c", Service: "d"}); got != "c" {
		t.Fatalf("serviceName should win when service_name empty, got %q", got)
	}
	if got := eventService(creditEvent{Source: "s"}); got != "s" {
		t.Fatalf("source fallback failed, got %q", got)
	}
}

// TestLastSpendMetric_NilEvent confirms a nil event produces no tile —
// the caller relies on this to skip when fetchLastCreditEvent returns
// (nil, nil) for an empty / unreachable response.
func TestLastSpendMetric_NilEvent(t *testing.T) {
	if m := lastSpendMetric(nil); m != nil {
		t.Fatalf("expected nil metric, got %+v", m)
	}
}

// TestLastSpendMetric_ZeroAmountSuppressesTile keeps free / refund
// rows from rendering as a $0.00 tile.
func TestLastSpendMetric_ZeroAmountSuppressesTile(t *testing.T) {
	if m := lastSpendMetric(&creditEvent{Service: "x"}); m != nil {
		t.Fatalf("zero-amount event should not produce a tile, got %+v", m)
	}
}

// TestLastSpendMetric_PopulatedFields verifies the shape of a real tile
// — value formatting, caption with service + relative age, and metric
// metadata the renderer consumes.
func TestLastSpendMetric_PopulatedFields(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	m := lastSpendMetric(&creditEvent{
		Timestamp:   ts,
		ServiceName: "Codex Cloud",
		CreditsUsed: 0.42,
	})
	if m == nil {
		t.Fatal("expected metric")
	}
	if m.ID != "last-spend" || m.Label != "LAST" {
		t.Fatalf("metric id/label: %+v", m)
	}
	if m.Value != "$0.42" {
		t.Fatalf("value formatting: %q", m.Value)
	}
	if !strings.HasPrefix(m.Caption, "Codex Cloud") || !strings.Contains(m.Caption, "ago") {
		t.Fatalf("caption: %q", m.Caption)
	}
	if m.NumericUnit != "dollars" || m.NumericGoodWhen != "low" {
		t.Fatalf("numeric metadata: %+v", m)
	}
}

// TestRelativeAge_Buckets covers each bucket boundary the caption uses.
func TestRelativeAge_Buckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "now"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := relativeAge(c.d); got != c.want {
			t.Fatalf("relativeAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
