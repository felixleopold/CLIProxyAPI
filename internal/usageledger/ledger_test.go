package usageledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func newEnabledLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	l := New()
	l.Configure(&config.Config{UsageLedger: config.UsageLedgerConfig{Enabled: true, Path: path}}, "unused.yaml")
	t.Cleanup(func() { _ = l.Flush() })
	return l, path
}

func claudeRecord(key string, at time.Time) coreusage.Record {
	// Claude reports uncached input, cache read and cache write separately and
	// output_tokens already includes the 10 thinking tokens.
	return coreusage.Record{
		Provider: "claude", ExecutorType: "ClaudeExecutor", Model: "claude-opus-5-5", Alias: "opus", APIKey: key, RequestedAt: at,
		Detail: coreusage.Detail{
			InputTokens: 100, OutputTokens: 30, ReasoningTokens: 10, CacheReadTokens: 50, CacheCreationTokens: 20,
			TokenBreakdown: coreusage.NewIndependentTokenBreakdown(100, 50, 20, 20, 10, 0),
		},
	}
}

func TestRecordAggregatesAndNormalizesCacheTokens(t *testing.T) {
	l, _ := newEnabledLedger(t)
	at := time.Date(2026, 10, 12, 9, 41, 0, 0, time.FixedZone("x", 2*3600))

	l.Record(claudeRecord("key-a", at))
	l.Record(claudeRecord("key-a", at.Add(5*time.Minute)))
	// OpenAI/Codex input includes the 400 cached tokens; output includes reasoning.
	l.Record(coreusage.Record{
		Provider: "codex", ExecutorType: "CodexExecutor", Model: "gpt-6.1-sol", APIKey: "key-a", RequestedAt: at,
		Detail: coreusage.Detail{InputTokens: 1000, OutputTokens: 200, ReasoningTokens: 50, CacheReadTokens: 400},
	})
	l.Record(coreusage.Record{Provider: "codex", Model: "gpt-6.1-sol", RequestedAt: at, Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 1}})
	// A failure after generation is still billed; one without usage is not.
	failed := claudeRecord("key-a", at)
	failed.Failed = true
	l.Record(failed)
	l.Record(coreusage.Record{Provider: "claude", Model: "claude-opus-5-5", APIKey: "key-a", RequestedAt: at, Failed: true})

	got := l.Query(time.Time{}, time.Time{}, NewPriceTable(nil)).Buckets
	if len(got) != 3 {
		t.Fatalf("buckets = %d, want 3: %+v", len(got), got)
	}
	byKey := map[string]Bucket{}
	for _, b := range got {
		if !b.Hour.Equal(time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)) {
			t.Fatalf("hour = %v, want 07:00 UTC", b.Hour)
		}
		byKey[b.Provider+"|"+b.Fingerprint] = b.Bucket
	}
	fp := Fingerprint("key-a")
	if len(fp) != 12 {
		t.Fatalf("fingerprint %q is not 12 chars", fp)
	}
	want := map[string]Bucket{
		"claude|" + fp: {Requests: 3, InputTokens: 300, OutputTokens: 90, CacheReadTokens: 150, CacheWriteTokens: 60, ReasoningTokens: 30},
		"codex|" + fp:  {Requests: 1, InputTokens: 600, OutputTokens: 200, CacheReadTokens: 400, ReasoningTokens: 50},
		"codex|":       {Requests: 1, InputTokens: 10, OutputTokens: 1},
	}
	for k, w := range want {
		g := byKey[k]
		if g.Requests != w.Requests || g.InputTokens != w.InputTokens || g.OutputTokens != w.OutputTokens ||
			g.CacheReadTokens != w.CacheReadTokens || g.CacheWriteTokens != w.CacheWriteTokens || g.ReasoningTokens != w.ReasoningTokens {
			t.Errorf("%s = %+v, want %+v", k, g, w)
		}
	}
}

func TestPriceLookup(t *testing.T) {
	cacheRead := 0.25
	table := NewPriceTable([]config.UsageLedgerPrice{
		{Model: "claude-opus-5-5", Input: 5, Output: 25, CacheRead: &cacheRead},
		{Model: "my-model-*", Input: 1, Output: 2},
	})

	if p, ok := table.Lookup("claude-opus-5-5"); !ok || p.Source != SourceConfig || p.Input != 5 || p.CacheRead != 0.25 || p.CacheWrite != 5 {
		t.Errorf("config override = %+v ok=%v", p, ok)
	}
	if p, ok := table.Lookup("claude-opus-5-5-high"); !ok || p.Source != SourceBuiltin || p.Input != 4 {
		t.Errorf("builtin prefix = %+v ok=%v", p, ok)
	}
	if p, ok := table.Lookup("my-model-v2"); !ok || p.Source != SourceConfig || p.CacheRead != 1 {
		t.Errorf("config prefix = %+v ok=%v", p, ok)
	}
	if p, ok := table.Lookup("gpt-5.5-2026-04-23"); !ok || p.Model != "gpt-5.5" {
		t.Errorf("dated snapshot = %+v ok=%v", p, ok)
	}
	if p, ok := table.Lookup("gpt-5.5"); !ok || p.Model != "gpt-5.5" {
		t.Errorf("exact must beat nothing else = %+v ok=%v", p, ok)
	}
	for _, model := range []string{"gpt-5.5-pro", "codex-auto-review", "claude-3-7-sonnet-20250219", "unknown"} {
		if _, ok := table.Lookup(model); ok {
			t.Errorf("%s must be unpriced", model)
		}
	}
	if got := (Price{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}).Cost(1_000_000, 1_000_000, 1_000_000, 1_000_000); got != 36.75 {
		t.Errorf("cost = %v, want 36.75", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	l, path := newEnabledLedger(t)
	at := time.Date(2026, 10, 12, 7, 5, 0, 0, time.UTC)
	l.Record(claudeRecord("key-a", at))
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); len(data) == 0 || strings.Contains(string(data), "key-a") {
		t.Error("ledger file must be non-empty and never contain the raw key")
	}

	reloaded := New()
	reloaded.Configure(&config.Config{UsageLedger: config.UsageLedgerConfig{Enabled: true, Path: path}}, "unused.yaml")
	reloaded.Record(claudeRecord("key-a", at))
	got := reloaded.Query(time.Time{}, time.Time{}, NewPriceTable(nil))
	if len(got.Buckets) != 1 || got.Buckets[0].Requests != 2 || got.Buckets[0].InputTokens != 200 || !got.Buckets[0].Priced {
		t.Fatalf("reloaded = %+v", got)
	}
	if got.Buckets[0].CostUSD <= 0 {
		t.Errorf("cost = %v, want > 0", got.Buckets[0].CostUSD)
	}
}

func TestConfigureKeepsHistoryWhenFileUnreadable(t *testing.T) {
	l, path := newEnabledLedger(t)
	l.Record(claudeRecord("key-a", time.Now()))

	// Switching to a path that exists but cannot be read must not drop or
	// overwrite the current history.
	unreadable := filepath.Join(t.TempDir(), "dir-not-file")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	l.Configure(&config.Config{UsageLedger: config.UsageLedgerConfig{Enabled: true, Path: unreadable}}, "unused.yaml")
	if l.path != path || len(l.buckets) != 1 || !l.enabled {
		t.Fatalf("ledger switched despite unreadable target: path=%s buckets=%d", l.path, len(l.buckets))
	}
}
