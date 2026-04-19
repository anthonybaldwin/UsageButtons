// Package providers defines the provider interface, snapshot types,
// and the provider registry.
package providers

// MetricValue represents a single usage metric from a provider.
type MetricValue struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Name          string   `json:"name,omitempty"`
	Value         any      `json:"value"`           // number or string
	NumericValue  *float64 `json:"numericValue,omitempty"`
	NumericUnit   string   `json:"numericUnit,omitempty"`   // "percent"|"dollars"|"cents"|"count"
	NumericGoodWhen string `json:"numericGoodWhen,omitempty"` // "high"|"low"
	NumericMax    *float64 `json:"numericMax,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	Ratio         *float64 `json:"ratio,omitempty"` // 0..1
	Direction     string   `json:"direction,omitempty"` // "up"|"down"|"right"|"left"
	ResetInSeconds *float64 `json:"resetInSeconds,omitempty"`
	Caption       string   `json:"caption,omitempty"`
	RawCount      *int     `json:"rawCount,omitempty"`
	RawMax        *int     `json:"rawMax,omitempty"`
	UpdatedAt     string   `json:"updatedAt,omitempty"`
	Stale         *bool    `json:"stale,omitempty"`
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

// List returns all registered providers.
func List() []Provider {
	out := make([]Provider, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	return out
}
