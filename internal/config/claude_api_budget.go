package config

// ClaudeAPIBudget uses integer microdollars (1 USD = 1,000,000 units).
// Prices are microdollars per million tokens, so fractional-dollar rates remain exact.
type ClaudeAPIBudget struct {
	StatePath       string                    `yaml:"state-path" json:"state-path"`
	LimitMicroUSD   int64                     `yaml:"limit-micro-usd" json:"limit-micro-usd"`
	InitialMicroUSD int64                     `yaml:"initial-used-micro-usd" json:"initial-used-micro-usd"`
	MonthlyAnchor   string                    `yaml:"monthly-anchor" json:"monthly-anchor"`
	Timezone        string                    `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Models          map[string]ClaudeAPIPrice `yaml:"models" json:"models"`
	AllowedBetas    []string                  `yaml:"allowed-betas,omitempty" json:"allowed-betas,omitempty"`
}

// ClaudeAPIPrice must bound every accepted request, including any context-length tiers.
// MaxInputTokens is the provider's hard input/context ceiling, not a caller estimate.
type ClaudeAPIPrice struct {
	MaxInputTokens  int64 `yaml:"max-input-tokens" json:"max-input-tokens"`
	MaxOutputTokens int64 `yaml:"max-output-tokens" json:"max-output-tokens"`
	Input           int64 `yaml:"input-micro-usd-per-million" json:"input-micro-usd-per-million"`
	Output          int64 `yaml:"output-micro-usd-per-million" json:"output-micro-usd-per-million"`
	CacheWrite5m    int64 `yaml:"cache-write-5m-micro-usd-per-million" json:"cache-write-5m-micro-usd-per-million"`
	CacheWrite1h    int64 `yaml:"cache-write-1h-micro-usd-per-million" json:"cache-write-1h-micro-usd-per-million"`
	CacheRead       int64 `yaml:"cache-read-micro-usd-per-million" json:"cache-read-micro-usd-per-million"`
}
