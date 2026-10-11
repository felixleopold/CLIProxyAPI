package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeBudgetV8GroupAndKeyRoundTrip(t *testing.T) {
	raw := `config-version: 8
upstream:
  claude:
    api-budgets:
      shared:
        state-path: /volume/claude-budget.json
        limit-micro-usd: 100000000
        initial-used-micro-usd: 10000
        monthly-anchor: "2026-10-11T00:00:00"
        timezone: UTC
api-keys:
  claude:
    - name: anthropic-credit-fallback
      budget-id: shared
      priority: -10
      keys:
        - api-key: fake-one
        - api-key: fake-two
        - api-key: fake-three
          budget-id: separate
`
	if err := ValidateV8Config([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClaudeKey) != 3 || cfg.ClaudeKey[0].BudgetID != "shared" || cfg.ClaudeKey[1].BudgetID != "shared" || cfg.ClaudeKey[2].BudgetID != "separate" {
		t.Fatalf("group/key budget selection was lost: %#v", cfg.ClaudeKey)
	}
	if cfg.ForAPIKey().Claude.APIBudgets["shared"].InitialMicroUSD != 10000 {
		t.Fatal("API-key config scope lost budget settings")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ClaudeKey[1].BudgetID != "shared" || reloaded.ClaudeKey[2].BudgetID != "separate" || reloaded.Claude.APIBudgets["shared"].LimitMicroUSD != 100000000 {
		t.Fatal("config persistence lost budget or overrides")
	}
	if err = ValidateV8Config([]byte(strings.ReplaceAll(raw, "  claude:\n    - name:", "  codex:\n    - name:"))); err == nil {
		t.Fatal("Claude group budget accepted for another provider")
	}
}
