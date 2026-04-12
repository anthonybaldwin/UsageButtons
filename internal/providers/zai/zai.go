package zai

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches z.ai usage data.
type Provider struct{}

func (Provider) ID() string         { return "zai" }
func (Provider) Name() string       { return "z.ai" }
func (Provider) BrandColor() string { return "#e85a6a" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
