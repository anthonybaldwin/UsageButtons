package claude

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Claude usage data.
type Provider struct{}

func (Provider) ID() string         { return "claude" }
func (Provider) Name() string       { return "Claude" }
func (Provider) BrandColor() string { return "#cc7c5e" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
