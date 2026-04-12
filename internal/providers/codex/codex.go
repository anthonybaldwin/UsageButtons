package codex

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Codex usage data.
type Provider struct{}

func (Provider) ID() string         { return "codex" }
func (Provider) Name() string       { return "Codex" }
func (Provider) BrandColor() string { return "#49a3b0" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
