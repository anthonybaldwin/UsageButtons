package kimik2

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Kimi K2 usage data.
type Provider struct{}

func (Provider) ID() string         { return "kimi-k2" }
func (Provider) Name() string       { return "Kimi K2" }
func (Provider) BrandColor() string { return "#4c00ff" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
