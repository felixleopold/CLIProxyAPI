package usageledger

import (
	"context"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func init() {
	coreusage.RegisterPlugin(plugin{})
}

// plugin feeds the default ledger independently of usage-statistics-enabled and
// of whether anyone consumes the redis usage queue.
type plugin struct{}

func (plugin) HandleUsage(_ context.Context, record coreusage.Record) {
	defaultLedger.Record(record)
}
