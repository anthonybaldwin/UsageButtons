package warp

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Warp usage data.
type Provider struct{}

func (Provider) ID() string         { return "warp" }
func (Provider) Name() string       { return "Warp" }
func (Provider) BrandColor() string { return "#938bb4" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
