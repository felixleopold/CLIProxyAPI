//go:build unix

package helps

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func budgetTestConfig(t *testing.T) (config.ClaudeAPIBudget, config.ClaudeAPIPrice, time.Time) {
	t.Helper()
	p := config.ClaudeAPIPrice{MaxInputTokens: 100, MaxOutputTokens: 100, Input: 4_000_000,
		Output: 20_000_000, CacheWrite5m: 5_000_000, CacheWrite1h: 8_000_000, CacheRead: 200_000}
	b := config.ClaudeAPIBudget{StatePath: filepath.Join(t.TempDir(), "budget.json"), LimitMicroUSD: 10_000,
		InitialMicroUSD: 10, MonthlyAnchor: "2026-10-11T00:00:00", Models: map[string]config.ClaudeAPIPrice{"test-model": p}}
	return b, p, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
}

func readBudgetTestState(t *testing.T, b config.ClaudeAPIBudget) ClaudeBudgetState {
	t.Helper()
	data, err := os.ReadFile(b.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var s ClaudeBudgetState
	if err = json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaudeBudgetConcurrentBoundaryAndRestart(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	b.LimitMicroUSD = 10 + 3*1000 // 100 input at the 1h rate, plus 10 output.
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			if _, err := ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("accepted %d, want exactly 3", accepted.Load())
	}
	s := readBudgetTestState(t, b)
	if s.UsedMicroUSD != b.LimitMicroUSD || len(s.Pending) != 3 {
		t.Fatalf("unexpected durable state: %#v", s)
	}
	// There is no in-memory ledger to reset on restart or credential rotation.
	if _, err := ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("pending reservations were lost")
	}
	b.InitialMicroUSD = 0
	if _, err := ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("changed seed reset usage")
	}
}

func TestClaudeBudgetRetryAndDuplicateSettlement(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	first, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Settle(40, now); err != nil {
		t.Fatal(err)
	}
	if err = first.Settle(0, now); err != nil {
		t.Fatal(err)
	}
	s := readBudgetTestState(t, b)
	if s.UsedMicroUSD != 1050 || len(s.Pending) != 1 || s.Pending[second.requestID] != 1000 {
		t.Fatalf("retry or duplicate settlement lost charges: %#v", s)
	}
}

func TestClaudeBudgetRolloverAndAdjustmentPreserveSpend(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	settled, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = settled.Settle(50, now); err != nil {
		t.Fatal(err)
	}
	pending, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	b.LimitMicroUSD = 1000
	b.MonthlyAnchor = "2026-10-10T12:00:00"
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("cap/schedule edit erased spend")
	}
	s := readBudgetTestState(t, b)
	if s.UsedMicroUSD != 1060 || s.NextRenewal != time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("bad adjusted state: %#v", s)
	}
	// An older in-flight request must not restore its captured schedule or limit.
	if err = pending.Settle(20, now); err != nil {
		t.Fatal(err)
	}
	s = readBudgetTestState(t, b)
	if s.UsedMicroUSD != 80 || s.LimitMicroUSD != 1000 || s.Schedule != b.MonthlyAnchor+"@UTC" {
		t.Fatalf("settlement reverted settings: %#v", s)
	}
	b.LimitMicroUSD = 10_000
	_, err = ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ReserveClaudeBudget("shared", b, p, 10, s.NextRenewal)
	if err != nil {
		t.Fatal(err)
	}
	s = readBudgetTestState(t, b)
	if s.UsedMicroUSD != 2000 || len(s.Pending) != 2 {
		t.Fatalf("renewal must drop settled spend but carry pending: %#v", s)
	}
}

func TestClaudeBudgetSettlementFirstAfterAdjustedRenewal(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	pending, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	// Persist settings that differ from the in-flight reservation's snapshot.
	b.LimitMicroUSD = 1000
	b.MonthlyAnchor = "2026-10-10T12:00:00"
	b.Timezone = "Europe/Amsterdam"
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("lowered cap admitted another request")
	}
	renewal := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	if err = pending.Settle(20, renewal); err != nil {
		t.Fatal(err)
	}
	s := readBudgetTestState(t, b)
	if s.UsedMicroUSD != 20 || len(s.Pending) != 0 || s.LimitMicroUSD != 1000 ||
		s.CycleStart != renewal || s.NextRenewal != time.Date(2026, 12, 10, 11, 0, 0, 0, time.UTC) ||
		s.Schedule != b.MonthlyAnchor+"@Europe/Amsterdam" {
		t.Fatalf("settlement failed to renew using persisted settings: %#v", s)
	}
	if _, err = ReserveClaudeBudget("shared", b, p, 10, renewal); err == nil {
		t.Fatal("admission discarded post-renewal settlement cost")
	}
	if s = readBudgetTestState(t, b); s.UsedMicroUSD != 20 || len(s.Pending) != 0 {
		t.Fatalf("admission changed settled charges: %#v", s)
	}
}

func TestClaudeBudgetScheduleTimezoneAndShortMonth(t *testing.T) {
	b, _, _ := budgetTestConfig(t)
	b.MonthlyAnchor = "2026-01-31T12:00:00"
	b.Timezone = "Europe/Amsterdam"
	_, start, next, err := budgetSchedule(b, time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if start != time.Date(2026, 2, 28, 11, 0, 0, 0, time.UTC) || next != time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("wrong local month/DST boundaries: %v %v", start, next)
	}
}

func TestClaudeBudgetFailedWriteCorruptAndMissingState(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	r, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(b.StatePath)
	if err = os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	if err = r.Settle(0, now); err == nil {
		_ = os.Chmod(dir, 0700)
		t.Fatal("unwritable state released reservation")
	}
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		_ = os.Chmod(dir, 0700)
		t.Fatal("unwritable state permitted reservation")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if readBudgetTestState(t, b).UsedMicroUSD != 1010 {
		t.Fatal("write failure altered existing charges")
	}
	if err = os.WriteFile(b.StatePath, []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("corruption initialized a new allowance")
	}
	if err = os.Remove(b.StatePath); err != nil {
		t.Fatal(err)
	}
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now); err == nil {
		t.Fatal("deleted state initialized a new allowance")
	}
}

func TestClaudeBudgetSelectedKeysAndSubscriptionBypass(t *testing.T) {
	b, _, _ := budgetTestConfig(t)
	cfg := &config.Config{Claude: config.ClaudeConfig{APIBudgets: map[string]config.ClaudeAPIBudget{"shared": b}},
		ClaudeKey: []config.ClaudeKey{{APIKey: "fake-key", BudgetID: "shared"}}}
	for _, auth := range []*cliproxyauth.Auth{
		{Provider: "claude", Metadata: map[string]any{"access_token": "fake-oauth"}},
		{Provider: "codex", Attributes: map[string]string{"api_key": "fake-key"}},
		{Provider: "claude", Attributes: map[string]string{"api_key": "different-key"}},
	} {
		id, _, err := ClaudeBudgetForAuth(cfg, auth)
		if err != nil || id != "" {
			t.Fatalf("unselected auth was budgeted: %s %v", id, err)
		}
	}
	id, _, err := ClaudeBudgetForAuth(cfg, &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake-key"}})
	if err != nil || id != "shared" {
		t.Fatalf("configured key bypassed without synthesized attr: %s %v", id, err)
	}
	delete(cfg.Claude.APIBudgets, "shared")
	_, _, err = ClaudeBudgetForAuth(cfg, &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake-key"}})
	if err == nil {
		t.Fatal("missing budget config failed open")
	}
}

func TestClaudeBudgetRawUsageCacheTTLAndThinking(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	r, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	// 10*4 + 2*20 + 20*.2 + 3*5 + 4*8 = 131 microdollars.
	usage := `{"input_tokens":10,"output_tokens":2,"thinking_tokens":1,"output_tokens_details":{"thinking_tokens":1},"cache_read_input_tokens":20,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}`
	cost, err := r.usageCost([]byte(usage))
	if err != nil || cost != 131 {
		t.Fatalf("wrong cache/thinking cost: %d %v", cost, err)
	}
	if _, err = r.usageCost([]byte(`{"input_tokens":10,"output_tokens":2,"iterations":[{"type":"compaction"}]}`)); err == nil {
		t.Fatal("unpriced extra sampling accepted")
	}
}

func TestClaudeBudgetCompleteAndIncompleteResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			b, p, now := budgetTestConfig(t)
			r, err := ReserveClaudeBudget("shared", b, p, 10, now)
			if err != nil {
				t.Fatal(err)
			}
			payload := `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
			if !complete {
				payload = `{"type":"message","usage":{"input_tokens":10,"output_tokens":2}}`
			}
			if stream {
				payload = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"
				if complete {
					payload += "data: {\"type\":\"message_stop\"}\n\n"
				}
			}
			body := ObserveClaudeBudgetBody(io.NopCloser(strings.NewReader(payload)), r, stream)
			if _, err = io.Copy(io.Discard, body); err != nil {
				t.Fatal(err)
			}
			if err = body.Close(); err != nil {
				t.Fatal(err)
			}
			s := readBudgetTestState(t, b)
			want := int64(1010)
			if complete {
				want = 90
			}
			if s.UsedMicroUSD != want {
				t.Fatalf("stream=%t complete=%t got %d want %d", stream, complete, s.UsedMicroUSD, want)
			}
		}
	}
}

func TestClaudeBudgetPayloadRefusesUnpricedFeatures(t *testing.T) {
	b, _, _ := budgetTestConfig(t)
	base := `{"model":"test-model","max_tokens":10,"service_tier":"standard_only","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"local","input_schema":{"type":"object"}}],"thinking":{"type":"adaptive"}}`
	if _, _, err := ClaudeBudgetRequestPrice(b, []byte(base), ""); err != nil {
		t.Fatal(err)
	}
	for _, addition := range []string{
		`"speed":"fast"`, `"service_tier":"auto"`, `"inference_geo":"us"`, `"container":"new"`,
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]`,
		`"context_management":{"edits":[{"type":"compact_20260112"}]}`,
		`"cache_control":{"type":"ephemeral","ttl":"24h"}`,
	} {
		var fields, extra map[string]json.RawMessage
		if err := json.Unmarshal([]byte(base), &fields); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte("{"+addition+"}"), &extra); err != nil {
			t.Fatal(err)
		}
		for key, value := range extra {
			fields[key] = value
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ClaudeBudgetRequestPrice(b, payload, ""); err == nil {
			t.Fatalf("accepted unpriced feature: %s", addition)
		}
	}
	if _, _, err := ClaudeBudgetRequestPrice(b, []byte(base), "future-beta"); err == nil {
		t.Fatal("accepted unknown beta")
	}
	b.AllowedBetas = []string{"interleaved-thinking-2025-05-14"}
	if _, _, err := ClaudeBudgetRequestPrice(b, []byte(base), "interleaved-thinking-2025-05-14"); err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(base, `"model":"test-model"`, `"model":"test-model","model":"expensive-model"`, 1)
	if _, _, err := ClaudeBudgetRequestPrice(b, []byte(duplicate), ""); err == nil {
		t.Fatal("ambiguous duplicate model accepted")
	}
}

func TestClaudeBudgetUnpricedCompletedUsageBlocksUntilReconciled(t *testing.T) {
	b, p, now := budgetTestConfig(t)
	r, err := ReserveClaudeBudget("shared", b, p, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":101,"output_tokens":2}}`
	body := ObserveClaudeBudgetBody(io.NopCloser(strings.NewReader(payload)), r, false)
	if _, err = io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if err = body.Close(); err != nil {
		t.Fatal(err)
	}
	s := readBudgetTestState(t, b)
	if s.BlockedReason == "" || s.UsedMicroUSD != 1010 || len(s.Pending) != 1 {
		t.Fatalf("unpriced usage released reservation: %#v", s)
	}
	b.LimitMicroUSD *= 2
	if _, err = ReserveClaudeBudget("shared", b, p, 10, now.AddDate(0, 1, 0)); err == nil {
		t.Fatal("cap change or renewal cleared unpriced-usage block")
	}
}
