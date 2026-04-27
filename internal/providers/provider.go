// Package providers defines the provider interface, snapshot types,
// and the provider registry.
package providers

// MetricValue represents a single usage metric from a provider.
type MetricValue struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Name            string   `json:"name,omitempty"`
	Value           any      `json:"value"` // number or string
	NumericValue    *float64 `json:"numericValue,omitempty"`
	NumericUnit     string   `json:"numericUnit,omitempty"`     // "percent"|"dollars"|"cents"|"count"
	NumericGoodWhen string   `json:"numericGoodWhen,omitempty"` // "high"|"low"
	NumericMax      *float64 `json:"numericMax,omitempty"`
	Unit            string   `json:"unit,omitempty"`
	Ratio           *float64 `json:"ratio,omitempty"`     // 0..1
	Direction       string   `json:"direction,omitempty"` // "up"|"down"|"right"|"left"
	ResetInSeconds  *float64 `json:"resetInSeconds,omitempty"`
	Caption         string   `json:"caption,omitempty"`
	RawCount        *int     `json:"rawCount,omitempty"`
	RawMax          *int     `json:"rawMax,omitempty"`
	UpdatedAt       string   `json:"updatedAt,omitempty"`
	Stale           *bool    `json:"stale,omitempty"`
}

// NumericVal returns the numeric value or 0.
func (m MetricValue) NumericVal() float64 {
	if m.NumericValue != nil {
		return *m.NumericValue
	}
	return 0
}

// RatioVal returns the ratio or -1 if not set.
func (m MetricValue) RatioVal() float64 {
	if m.Ratio != nil {
		return *m.Ratio
	}
	return -1
}

// Snapshot is the result of a single provider fetch.
type Snapshot struct {
	ProviderID   string        `json:"providerId"`
	ProviderName string        `json:"providerName"`
	Source       string        `json:"source"` // "mock"|"oauth"|"web"|"cli"|"cache"
	Metrics      []MetricValue `json:"metrics"`
	Status       string        `json:"status,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// FetchContext provides context to a provider's Fetch method.
type FetchContext struct {
	PollIntervalMs int64
	Force          bool
	// ActiveMetricIDs is the sorted, deduped set of metric IDs that at
	// least one currently-bound Stream Deck button is displaying for
	// this provider. Providers MAY use it to skip endpoints whose data
	// doesn't contribute to any listed metric — saves quota on multi-
	// endpoint providers (Perplexity, Gemini, Vertex, Cursor, Claude)
	// for users who only bind a subset of their available metrics.
	//
	// Semantics:
	//   nil           — fetch everything (cold start, force-refresh,
	//                   or any context where the active set isn't known)
	//   non-nil empty — no buttons bound; cache normally won't call
	//                   Fetch in this state, but if it does, the
	//                   provider may return an empty snapshot
	//   non-nil set   — provider may skip work for metrics not listed
	//
	// Providers that ignore this field continue to work — the refactor
	// is opt-in per provider, so adding it here is a no-op for the
	// existing fleet.
	ActiveMetricIDs []string
}

// Provider is the interface every usage-data source implements.
type Provider interface {
	ID() string
	Name() string
	BrandColor() string
	BrandBg() string
	MetricIDs() []string
	Fetch(ctx FetchContext) (Snapshot, error)
}

// --- Registry ---

// registry holds the process-wide set of providers keyed by ID().
var registry = map[string]Provider{}

// Register adds a provider to the global registry.
func Register(p Provider) {
	registry[p.ID()] = p
}

// Get returns a provider by ID, or nil.
func Get(id string) Provider {
	return registry[id]
}
