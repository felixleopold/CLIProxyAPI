package usageledger

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Price sources reported by the prices endpoint.
const (
	SourceBuiltin = "builtin"
	SourceConfig  = "config"
)

// Price is a USD per 1M tokens rate card. Model is an exact id, or a prefix with
// a trailing '*'.
type Price struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Source     string  `json:"source"`
}

// Built-in prices, USD per 1M tokens. Verified 2026-10-16 against:
//   - Anthropic: https://platform.claude.com/docs/en/about-claude/pricing
//   - OpenAI (Standard tier, context up to 272K input tokens):
//     https://developers.openai.com/api/docs/pricing
//
// Deliberate simplifications:
//   - Claude cache writes use the 5 minute rate; usage records do not split 5m/1h.
//   - Claude Haiku 5.5 uses the over-100K-prompt tier for every request (the
//     page has a cheaper tier up to 100K prompt tokens), so it overcharges.
//   - OpenAI long-context (>272K) surcharges are not modeled.
//   - gpt-5.5 has no separate cache-write rate; writes cost the input rate.
//   - Models absent from the pages are unpriced on purpose: claude-3-7-sonnet-*,
//     codex-auto-review. Unpriced models are reported, never guessed.
var builtinPrices = []Price{
	{Model: "claude-fable-5-1", Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	{Model: "claude-fable-5", Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	{Model: "claude-opus-5-5", Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	{Model: "claude-opus-5-5-*", Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	{Model: "claude-opus-5", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-8", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-7", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-6", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-6-*", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-5*", Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	{Model: "claude-opus-4-1*", Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	{Model: "claude-opus-4-20250514", Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	{Model: "claude-sonnet-5-5", Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	{Model: "claude-sonnet-5-5-*", Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	{Model: "claude-sonnet-5", Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	{Model: "claude-sonnet-4-6", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	{Model: "claude-sonnet-4-5*", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	{Model: "claude-sonnet-4-20250514", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	{Model: "claude-haiku-5-5", Input: 0.5, Output: 2.5, CacheRead: 0.05, CacheWrite: 0.625},
	{Model: "claude-haiku-4-5*", Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	{Model: "claude-3-5-haiku*", Input: 0.8, Output: 4, CacheRead: 0.08, CacheWrite: 1},

	{Model: "gpt-6-astra", Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	{Model: "gpt-6.1-sol", Input: 2, Output: 10, CacheRead: 0.1, CacheWrite: 2.5},
	{Model: "gpt-6-sol", Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	{Model: "gpt-6-luna", Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
	{Model: "gpt-5.6-terra", Input: 2, Output: 12, CacheRead: 0.2, CacheWrite: 2.5},
	{Model: "gpt-5.6-luna", Input: 0.2, Output: 1.2, CacheRead: 0.02, CacheWrite: 0.25},
	{Model: "gpt-5.5", Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 5},
}

// PriceTable resolves model ids to prices; config entries override built-ins.
type PriceTable struct {
	entries []Price
}

// NewPriceTable merges the built-in table with config overrides.
func NewPriceTable(overrides []config.UsageLedgerPrice) *PriceTable {
	entries := make([]Price, 0, len(builtinPrices)+len(overrides))
	for _, p := range builtinPrices {
		p.Source = SourceBuiltin
		entries = append(entries, p)
	}
	for _, o := range overrides {
		model := strings.TrimSpace(o.Model)
		if model == "" {
			continue
		}
		p := Price{Model: model, Input: o.Input, Output: o.Output, CacheRead: o.Input, CacheWrite: o.Input, Source: SourceConfig}
		if o.CacheRead != nil {
			p.CacheRead = *o.CacheRead
		}
		if o.CacheWrite != nil {
			p.CacheWrite = *o.CacheWrite
		}
		entries = append(entries, p)
	}
	return &PriceTable{entries: entries}
}

// Entries returns the effective table. A config entry replaces a built-in one
// with the same model pattern.
func (t *PriceTable) Entries() []Price {
	index := make(map[string]int, len(t.entries))
	out := make([]Price, 0, len(t.entries))
	for _, p := range t.entries {
		if i, ok := index[p.Model]; ok {
			out[i] = p
			continue
		}
		index[p.Model] = len(out)
		out = append(out, p)
	}
	return out
}

// Lookup returns the price for model: an exact match wins, then the longest
// matching prefix pattern; config beats built-in at equal specificity.
func (t *PriceTable) Lookup(model string) (Price, bool) {
	model = strings.TrimSpace(model)
	var best Price
	bestScore := -1
	for _, p := range t.entries {
		score := -1
		if prefix, ok := strings.CutSuffix(p.Model, "*"); ok {
			if strings.HasPrefix(model, prefix) {
				score = len(prefix)
			}
		} else if p.Model == model {
			score = 1 << 20
		}
		// >= lets later (config) entries win ties.
		if score >= 0 && score >= bestScore {
			best, bestScore = p, score
		}
	}
	return best, bestScore >= 0
}

// Cost returns USD for the given token counts at this rate.
func (p Price) Cost(input, output, cacheRead, cacheWrite int64) float64 {
	return (float64(input)*p.Input + float64(output)*p.Output +
		float64(cacheRead)*p.CacheRead + float64(cacheWrite)*p.CacheWrite) / 1e6
}
