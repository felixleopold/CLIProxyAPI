package quotahistory

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var testNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func newEnabled(t *testing.T) (*History, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.json")
	h := New()
	h.now = func() time.Time { return testNow }
	h.Configure(&config.Config{QuotaHistory: config.QuotaHistoryConfig{Enabled: true, Path: path}}, "unused.yaml")
	return h, path
}

func claudeAuth(observed time.Time) *coreauth.Auth {
	return &coreauth.Auth{ID: "claude-1", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.42",
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1792300000",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1792200000",
	}}}
}

func codexAuth(observed time.Time) *coreauth.Auth {
	return &coreauth.Auth{ID: "codex-1", Provider: "codex", Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":           "48",
		"X-Codex-Primary-Window-Minutes":         "10080",
		"X-Codex-Primary-Reset-At":               "1792300000",
		"X-Codex-Secondary-Used-Percent":         "3",
		"X-Codex-Secondary-Window-Minutes":       "300",
		"X-Codex-Secondary-Reset-After-Seconds":  "600",
		"X-Codex-Bengalfox-Primary-Used-Percent": "99",
	}}}
}

func TestParseSamplesClaudeAndCodex(t *testing.T) {
	at := testNow.Add(-time.Minute)
	claude := parseSamples(claudeAuth(at))
	if len(claude) != 2 {
		t.Fatalf("claude samples = %+v", claude)
	}
	weekly, session := claude[0], claude[1]
	if weekly.Window != "weekly" || weekly.WindowMinutes != 10080 || weekly.UsedPercent != 42 || weekly.ResetAt.Unix() != 1792300000 || !weekly.At.Equal(at) || weekly.AuthID != "claude-1" || weekly.Provider != "claude" {
		t.Errorf("claude weekly = %+v", weekly)
	}
	if session.Window != "session" || session.WindowMinutes != 300 || session.UsedPercent != 10 {
		t.Errorf("claude session = %+v", session)
	}

	codex := parseSamples(codexAuth(at))
	if len(codex) != 2 {
		t.Fatalf("codex samples = %+v", codex)
	}
	if codex[0].Window != "weekly" || codex[0].UsedPercent != 48 || codex[0].ResetAt.Unix() != 1792300000 {
		t.Errorf("codex primary = %+v", codex[0])
	}
	if codex[1].Window != "session" || codex[1].WindowMinutes != 300 || !codex[1].ResetAt.Equal(at.Add(10*time.Minute)) {
		t.Errorf("codex secondary = %+v", codex[1])
	}
	if got := parseSamples(&coreauth.Auth{ID: "x", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: at, Signals: map[string]string{"Retry-After": "5"}}}); len(got) != 0 {
		t.Errorf("unparseable credential produced %+v", got)
	}
}

func TestSampleAppendsOnlyWhenObservedAtAdvances(t *testing.T) {
	h, path := newEnabled(t)
	observed := testNow.Add(-time.Minute)
	auths := []*coreauth.Auth{claudeAuth(observed), {ID: "empty", Provider: "claude"}}

	if got := h.Sample(auths); got != 2 {
		t.Fatalf("first pass added %d, want 2", got)
	}
	if got := h.Sample(auths); got != 0 {
		t.Fatalf("unchanged snapshot added %d, want 0", got)
	}
	auths[0] = claudeAuth(observed.Add(30 * time.Second))
	if got := h.Sample(auths); got != 2 {
		t.Fatalf("advanced snapshot added %d, want 2", got)
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}

	// After a restart the persisted samples stop the same snapshot being re-recorded.
	reloaded := New()
	reloaded.now = h.now
	reloaded.Configure(&config.Config{QuotaHistory: config.QuotaHistoryConfig{Enabled: true, Path: path}}, "unused.yaml")
	if got := reloaded.Sample(auths); got != 0 {
		t.Fatalf("reloaded pass added %d, want 0", got)
	}
	if got := len(reloaded.Query(time.Time{}, time.Time{})); got != 4 {
		t.Fatalf("reloaded samples = %d, want 4", got)
	}
}

func TestSamplePrunesOldSamples(t *testing.T) {
	h, _ := newEnabled(t)
	h.Sample([]*coreauth.Auth{claudeAuth(testNow.Add(-36 * 24 * time.Hour))})
	if got := len(h.Query(time.Time{}, time.Time{})); got != 0 {
		t.Fatalf("expired samples kept: %d", got)
	}
	h.Sample([]*coreauth.Auth{claudeAuth(testNow.Add(-34 * 24 * time.Hour))})
	if got := len(h.Query(time.Time{}, time.Time{})); got != 2 {
		t.Fatalf("recent samples = %d, want 2", got)
	}
}
