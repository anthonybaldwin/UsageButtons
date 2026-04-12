package copilot

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Copilot usage data.
type Provider struct{}

func (Provider) ID() string         { return "copilot" }
func (Provider) Name() string       { return "Copilot" }
func (Provider) BrandColor() string { return "#a855f7" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
