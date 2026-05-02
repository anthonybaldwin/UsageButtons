package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// creditUsageEventsPath is the chatgpt.com analytics endpoint that
// lists individual credit-usage transactions. It backs the "Credits
// usage history" table in /codex/cloud/settings/analytics. The endpoint
// is cookie-gated, so we only reach it through the companion extension.
const creditUsageEventsPath = "/wham/usage/credit-usage-events"

// creditEvent is one row from the credit-usage history. Field tags
// cover the snake_case + camelCase + abbreviated variants the chatgpt.com
// API has historically used so we don't have to redeploy on key renames.
type creditEvent struct {
	Timestamp   string  `json:"timestamp"`
	CreatedAt   string  `json:"created_at"`
	CreatedAtC  string  `json:"createdAt"`
	OccurredAt  string  `json:"occurred_at"`
	OccurredAtC string  `json:"occurredAt"`
	Service     string  `json:"service"`
	ServiceName string  `json:"service_name"`
	ServiceC    string  `json:"serviceName"`
	Source      string  `json:"source"`
	CreditsUsed float64 `json:"credits_used"`
	CreditsC    float64 `json:"creditsUsed"`
	Credits     float64 `json:"credits"`
	Amount      float64 `json:"amount"`
}

// creditEventsResponse handles the most likely top-level wrappings.
type creditEventsResponse struct {
	Events []creditEvent `json:"events"`
	Items  []creditEvent `json:"items"`
	Data   []creditEvent `json:"data"`
}

// fetchLastCreditEvent returns the newest credit-usage transaction via
// the companion extension. Returns nil,nil when the extension isn't
// reachable so the caller skips the metric without erroring the snapshot.
func fetchLastCreditEvent(base string) (*creditEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !cookies.HostAvailable(ctx) {
		return nil, nil
	}
	var raw json.RawMessage
	if err := cookies.FetchJSON(ctx, base+creditUsageEventsPath, nil, &raw); err != nil {
		return nil, err
	}
	if ev := parseCreditEvents(raw); ev != nil {
		return ev, nil
	}
	if providers.LogSink != nil {
		preview := string(raw)
		if len(preview) > 400 {
			preview = preview[:400] + "..."
		}
		providers.LogSink(fmt.Sprintf("[codex] credit-usage-events unknown shape: %s", preview))
	}
	return nil, nil
}

// parseCreditEvents tries the array, then common object wrappings, and
// returns the newest event by timestamp. Unknown shapes return nil so
// the caller can log the raw payload for diagnosis.
func parseCreditEvents(raw json.RawMessage) *creditEvent {
	var arr []creditEvent
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return pickNewestEvent(arr)
	}
	var obj creditEventsResponse
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, group := range [][]creditEvent{obj.Events, obj.Items, obj.Data} {
			if len(group) > 0 {
				return pickNewestEvent(group)
			}
		}
	}
	return nil
}

// pickNewestEvent returns the entry with the newest parseable timestamp.
// Falls back to the first row when no timestamp parses, since the API
// usually returns newest-first.
func pickNewestEvent(events []creditEvent) *creditEvent {
	var best *creditEvent
	var bestTS time.Time
	for i := range events {
		t, ok := parseEventTS(events[i])
		if !ok {
			continue
		}
		if best == nil || t.After(bestTS) {
			best = &events[i]
			bestTS = t
		}
	}
	if best == nil && len(events) > 0 {
		best = &events[0]
	}
	return best
}

// parseEventTS reads whichever timestamp field the row carries.
func parseEventTS(e creditEvent) (time.Time, bool) {
	for _, raw := range []string{e.Timestamp, e.CreatedAt, e.CreatedAtC, e.OccurredAt, e.OccurredAtC} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// eventAmount returns the dollar amount on a credit event, tolerating
// the multiple field-name variants the API has used.
func eventAmount(e creditEvent) float64 {
	for _, v := range []float64{e.CreditsUsed, e.CreditsC, e.Credits, e.Amount} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// eventService returns the service label for a credit event.
func eventService(e creditEvent) string {
	for _, v := range []string{e.ServiceName, e.ServiceC, e.Service, e.Source} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// lastSpendMetric turns a credit event into the "last-spend" tile.
// Returns nil if the event has no usable amount, so an empty/free
// transaction doesn't render a $0.00 tile.
func lastSpendMetric(ev *creditEvent) *providers.MetricValue {
	if ev == nil {
		return nil
	}
	amount := eventAmount(*ev)
	if amount <= 0 {
		return nil
	}
	caption := "Last spend"
	if service := eventService(*ev); service != "" {
		caption = service
	}
	if t, ok := parseEventTS(*ev); ok {
		caption = caption + " · " + relativeAge(time.Since(t))
	}
	return &providers.MetricValue{
		ID:              "last-spend",
		Label:           "LAST",
		Name:            "Most recent Codex credit transaction",
		Value:           fmt.Sprintf("$%.2f", amount),
		NumericValue:    &amount,
		NumericUnit:     "dollars",
		NumericGoodWhen: "low",
		Caption:         caption,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

// relativeAge formats a duration as a compact "Nm ago" / "Nh ago" / "Nd ago".
func relativeAge(d time.Duration) string {
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
