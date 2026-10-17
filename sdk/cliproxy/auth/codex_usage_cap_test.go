package auth

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSchedulerPick_CodexUsageCapSkipsCredentialUntilUsageDrops(t *testing.T) {
	t.Parallel()

	resetAt := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	capped := &Auth{
		ID: "plus", Provider: "codex",
		Attributes: map[string]string{"priority": "10"},
		Metadata:   map[string]any{MetadataUsageCapPercent: json.Number("80")},
		Quota: QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":   "12",
			"X-Codex-Primary-Reset-At":       resetAt,
			"X-Codex-Secondary-Used-Percent": "80",
			"X-Codex-Secondary-Reset-At":     resetAt,
		}},
	}
	// The uncapped credential stays eligible even at full usage.
	uncapped := &Auth{
		ID: "pro", Provider: "codex",
		Attributes: map[string]string{"priority": "0"},
		Quota: QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "100",
			"X-Codex-Primary-Reset-At":     resetAt,
		}},
	}
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, capped, uncapped)

	pick := func() string {
		t.Helper()
		got, errPick := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil || got == nil {
			t.Fatalf("pickSingle() = %v, %v", got, errPick)
		}
		return got.ID
	}
	if got := pick(); got != "pro" {
		t.Fatalf("capped credential picked: got %q, want %q", got, "pro")
	}

	released := capped.Clone()
	released.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "79"
	scheduler.upsertAuth(released)
	if got := pick(); got != "plus" {
		t.Fatalf("credential below cap not picked: got %q, want %q", got, "plus")
	}
}
