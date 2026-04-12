package openrouter

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches OpenRouter usage data.
type Provider struct{}

func (Provider) ID() string         { return "openrouter" }
func (Provider) Name() string       { return "OpenRouter" }
func (Provider) BrandColor() string { return "#6467f2" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
