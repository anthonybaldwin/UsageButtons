package cursor

import (
	"fmt"

	"github.com/anthonybaldwin/UsageButtons/internal/providers"
)

// Provider fetches Cursor usage data.
type Provider struct{}

func (Provider) ID() string         { return "cursor" }
func (Provider) Name() string       { return "Cursor" }
func (Provider) BrandColor() string { return "#00bfa5" }
func (Provider) MetricIDs() []string { return []string{} } // TODO: fill in
func (Provider) Fetch(_ providers.FetchContext) (providers.Snapshot, error) {
	return providers.Snapshot{}, fmt.Errorf("not configured")
}

func init() {
	providers.Register(Provider{})
}
