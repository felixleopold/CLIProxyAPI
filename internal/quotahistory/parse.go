package quotahistory

import (
	"math"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	windowWeekly  = "weekly"
	windowSession = "session"
	// weeklyMinWindowMinutes is the smallest window reported as "weekly".
	weeklyMinWindowMinutes = 6 * 24 * 60
)

// Sample is one credential's usage of one quota window at one instant.
type Sample struct {
	At            time.Time `json:"at"`
	AuthID        string    `json:"auth_id"`
	Provider      string    `json:"provider"`
	Window        string    `json:"window"`
	WindowMinutes int       `json:"window_minutes"`
	UsedPercent   float64   `json:"used_percent"`
	ResetAt       time.Time `json:"reset_at"`
}

// parseSamples extracts every complete quota window from a credential's passive
// snapshot. Windows missing a value or with an unparseable one are skipped.
func parseSamples(auth *coreauth.Auth) []Sample {
	if auth == nil || auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		return nil
	}
	at := auth.Quota.ObservedAt.UTC()
	signals := auth.Quota.Signals
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))

	var out []Sample
	add := func(minutes int, used float64, reset time.Time) {
		if minutes <= 0 || reset.IsZero() || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			return
		}
		window := windowSession
		if minutes >= weeklyMinWindowMinutes {
			window = windowWeekly
		}
		out = append(out, Sample{At: at, AuthID: auth.ID, Provider: provider, Window: window, WindowMinutes: minutes, UsedPercent: used, ResetAt: reset.UTC()})
	}

	switch provider {
	case "claude":
		// Utilization is a 0..1 fraction; reset is unix seconds.
		for _, w := range []struct {
			name    string
			minutes int
		}{{"7d", 7 * 24 * 60}, {"5h", 5 * 60}} {
			prefix := "Anthropic-Ratelimit-Unified-" + w.name + "-"
			used, okUsed := parseFloat(signals[prefix+"Utilization"])
			reset, okReset := parseUnix(signals[prefix+"Reset"])
			if okUsed && okReset {
				add(w.minutes, used*100, reset)
			}
		}
	case "codex":
		// Plain primary/secondary windows only; namespaced extra limits are ignored.
		for _, name := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + name + "-"
			used, okUsed := parseFloat(signals[prefix+"Used-Percent"])
			minutes, okMinutes := parseFloat(signals[prefix+"Window-Minutes"])
			reset, okReset := parseUnix(signals[prefix+"Reset-At"])
			if !okReset {
				if after, ok := parseFloat(signals[prefix+"Reset-After-Seconds"]); ok {
					reset, okReset = at.Add(time.Duration(after*float64(time.Second))), true
				}
			}
			if okUsed && okMinutes && okReset {
				add(int(minutes), used, reset)
			}
		}
	}
	return out
}

func parseFloat(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return v, err == nil
}

func parseUnix(raw string) (time.Time, bool) {
	sec, ok := parseFloat(raw)
	if !ok || sec <= 0 || math.IsInf(sec, 0) {
		return time.Time{}, false
	}
	whole := math.Floor(sec)
	return time.Unix(int64(whole), int64((sec-whole)*1e9)), true
}
