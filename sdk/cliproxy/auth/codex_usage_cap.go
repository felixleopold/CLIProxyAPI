package auth

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// MetadataUsageCapPercent is the auth-file key that caps how much of each
// Codex rate-limit window (5-hour primary and weekly secondary) a credential
// may consume before it stops receiving requests. Values outside (0, 100)
// disable the cap.
const MetadataUsageCapPercent = "usage_cap_percent"

// codexUsageCapBlock reports whether a Codex credential's last observed
// primary or secondary window usage has reached its configured cap, and when
// the latest such window resets. The gate is derived from the passive quota
// snapshot on every check, so it lifts on its own once the window resets or a
// newer observation shows usage below the cap.
func codexUsageCapBlock(auth *Auth, now time.Time) (bool, time.Time) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false, time.Time{}
	}
	limit, ok := usageCapPercent(auth.Metadata)
	if !ok {
		return false, time.Time{}
	}
	var until time.Time
	for _, window := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + window + "-"
		used, errParse := strconv.ParseFloat(auth.Quota.Signals[prefix+"Used-Percent"], 64)
		if errParse != nil || used < limit {
			continue
		}
		if reset := codexWindowResetAt(auth.Quota, prefix); reset.After(now) && reset.After(until) {
			until = reset
		}
	}
	return !until.IsZero(), until
}

// UsageCapReached reports whether a credential is held back by its configured
// usage cap. Callers outside the scheduler use it to keep pins consistent.
func UsageCapReached(auth *Auth, now time.Time) bool {
	capped, _ := codexUsageCapBlock(auth, now)
	return capped
}

func usageCapPercent(metadata map[string]any) (float64, bool) {
	var limit float64
	switch value := metadata[MetadataUsageCapPercent].(type) {
	case float64:
		limit = value
	case json.Number:
		parsed, errParse := value.Float64()
		if errParse != nil {
			return 0, false
		}
		limit = parsed
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if errParse != nil {
			return 0, false
		}
		limit = parsed
	default:
		return 0, false
	}
	return limit, limit > 0 && limit < 100
}

// codexWindowResetAt prefers the absolute reset timestamp and falls back to the
// relative reset delay measured from the observation time.
func codexWindowResetAt(quota QuotaState, prefix string) time.Time {
	if resetAt, errParse := strconv.ParseInt(quota.Signals[prefix+"Reset-At"], 10, 64); errParse == nil && resetAt > 0 {
		return time.Unix(resetAt, 0)
	}
	if after, errParse := strconv.ParseInt(quota.Signals[prefix+"Reset-After-Seconds"], 10, 64); errParse == nil && after >= 0 && !quota.ObservedAt.IsZero() {
		return quota.ObservedAt.Add(time.Duration(after) * time.Second)
	}
	return time.Time{}
}
