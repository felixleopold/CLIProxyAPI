package helps

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type claudeBudgetError struct {
	code    int
	message string
}

func (e *claudeBudgetError) Error() string   { return e.message }
func (e *claudeBudgetError) StatusCode() int { return e.code }

// Local admission failures must not cool otherwise valid provider credentials.
func (e *claudeBudgetError) IsRequestScoped() bool { return true }

func budgetError(code int, format string, args ...any) error {
	return &claudeBudgetError{code, "claude API budget: " + fmt.Sprintf(format, args...)}
}

// ClaudeBudgetForAuth never applies to subscription OAuth. Matching configured keys
// also protects callers that construct Auth directly without synthesized attributes.
func ClaudeBudgetForAuth(cfg *config.Config, auth *cliproxyauth.Auth) (string, config.ClaudeAPIBudget, error) {
	if cfg == nil || auth == nil || (!strings.EqualFold(auth.Provider, "claude") && auth.Provider != "") ||
		auth.AuthKind() != cliproxyauth.AuthKindAPIKey {
		return "", config.ClaudeAPIBudget{}, nil
	}
	id := auth.Attributes["claude_budget_id"]
	for _, key := range cfg.ClaudeKey {
		if strings.TrimSpace(key.APIKey) != "" && strings.TrimSpace(key.APIKey) == auth.Attributes["api_key"] && key.BudgetID != "" {
			if id != "" && id != key.BudgetID {
				return "", config.ClaudeAPIBudget{}, budgetError(503, "credential has conflicting budget IDs")
			}
			id = key.BudgetID
		}
	}
	if id == "" {
		return "", config.ClaudeAPIBudget{}, nil
	}
	b, exists := cfg.Claude.APIBudgets[id]
	if !exists {
		return "", b, budgetError(503, "selected allowance is missing")
	}
	return id, b, nil
}

// ClaudeBudgetState is also the on-disk, secret-free status format. UsedMicroUSD
// includes Pending; an interrupted request keeps its entire reservation charged.
type ClaudeBudgetState struct {
	Version       int              `json:"version"`
	ID            string           `json:"id"`
	Schedule      string           `json:"schedule"`
	CycleStart    time.Time        `json:"cycle_start"`
	NextRenewal   time.Time        `json:"next_renewal"`
	UsedMicroUSD  int64            `json:"used_micro_usd"`
	LimitMicroUSD int64            `json:"limit_micro_usd"`
	Pending       map[string]int64 `json:"pending"`
	BlockedReason string           `json:"blocked_reason,omitempty"`
}

// ClaudeBudgetReservation captures prices at send time, unaffected by hot reload.
type ClaudeBudgetReservation struct {
	id        string
	config    config.ClaudeAPIBudget
	requestID string
	price     config.ClaudeAPIPrice
	maxOutput int64
}

func budgetSchedule(b config.ClaudeAPIBudget, now time.Time) (string, time.Time, time.Time, error) {
	zone := b.Timezone
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return "", time.Time{}, time.Time{}, fmt.Errorf("invalid timezone")
	}
	anchor, err := time.ParseInLocation("2006-01-02T15:04:05", b.MonthlyAnchor, loc)
	if err != nil || now.Before(anchor) {
		return "", time.Time{}, time.Time{}, fmt.Errorf("invalid or future monthly anchor")
	}
	nowLocal := now.In(loc)
	months := (nowLocal.Year()-anchor.Year())*12 + int(nowLocal.Month()-anchor.Month())
	boundary := func(offset int) time.Time {
		month := time.Date(anchor.Year(), anchor.Month()+time.Month(offset), 1, anchor.Hour(), anchor.Minute(), anchor.Second(), 0, loc)
		last := time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, loc).Day()
		day := anchor.Day()
		if day > last {
			day = last
		}
		return time.Date(month.Year(), month.Month(), day, anchor.Hour(), anchor.Minute(), anchor.Second(), 0, loc)
	}
	start := boundary(months)
	if now.Before(start) {
		months--
		start = boundary(months)
	}
	return b.MonthlyAnchor + "@" + zone, start.UTC(), boundary(months + 1).UTC(), nil
}

func validateBudgetConfig(b config.ClaudeAPIBudget) error {
	if !filepath.IsAbs(b.StatePath) || b.LimitMicroUSD <= 0 || b.LimitMicroUSD > 1_000_000_000_000_000 ||
		b.InitialMicroUSD < 0 || b.InitialMicroUSD > 1_000_000_000_000_000 {
		return fmt.Errorf("absolute state-path and valid positive limit/nonnegative initial usage are required")
	}
	return nil
}

func pendingBudgetTotal(s *ClaudeBudgetState) (int64, error) {
	var total int64
	for id, amount := range s.Pending {
		if id == "" || amount <= 0 || amount > math.MaxInt64-total {
			return 0, fmt.Errorf("invalid pending reservations")
		}
		total += amount
	}
	return total, nil
}

// Each transaction takes an OS file lock and reloads disk state, so independent
// executors and processes on the same local volume cannot create extra allowances.
func budgetTransaction(id string, b config.ClaudeAPIBudget, now time.Time, reconcile bool, mutate func(*ClaudeBudgetState) error) error {
	if err := validateBudgetConfig(b); err != nil {
		return budgetError(503, "%v", err)
	}
	var schedule string
	var start, next time.Time
	if reconcile {
		var errSchedule error
		schedule, start, next, errSchedule = budgetSchedule(b, now)
		if errSchedule != nil {
			return budgetError(503, "%v", errSchedule)
		}
	}
	unlock, fresh, err := lockClaudeBudget(b.StatePath + ".lock")
	if err != nil {
		return budgetError(503, "cannot lock durable state: %v", err)
	}
	defer unlock()
	var s ClaudeBudgetState
	data, err := os.ReadFile(b.StatePath)
	if os.IsNotExist(err) && fresh && reconcile {
		s = ClaudeBudgetState{Version: 1, ID: id, Schedule: schedule, CycleStart: start, NextRenewal: next,
			UsedMicroUSD: b.InitialMicroUSD, Pending: map[string]int64{}}
	} else {
		if err != nil {
			return budgetError(503, "cannot read durable state: %v", err)
		}
		if err = json.Unmarshal(data, &s); err != nil {
			return budgetError(503, "corrupt durable state")
		}
		pending, errPending := pendingBudgetTotal(&s)
		if errPending != nil || s.Version != 1 || s.ID != id || s.Schedule == "" || s.Pending == nil ||
			s.UsedMicroUSD < pending || s.CycleStart.IsZero() || !s.NextRenewal.After(s.CycleStart) {
			return budgetError(503, "invalid durable state")
		}
		if reconcile && s.Schedule != schedule {
			// A schedule edit carries spend forward, even if its new anchor describes
			// a different current cycle. Its next future boundary becomes effective.
			if !next.After(s.CycleStart) {
				return budgetError(503, "schedule change precedes the persisted cycle")
			}
			s.Schedule, s.NextRenewal = schedule, next
		} else if !now.Before(s.NextRenewal) {
			if !reconcile {
				// Settlement may be the first transaction after renewal. Use the
				// persisted schedule, never the old reservation's captured settings.
				anchor, zone, valid := strings.Cut(s.Schedule, "@")
				if !valid {
					return budgetError(503, "invalid persisted renewal schedule")
				}
				var errSchedule error
				_, start, next, errSchedule = budgetSchedule(config.ClaudeAPIBudget{MonthlyAnchor: anchor, Timezone: zone}, now)
				if errSchedule != nil {
					return budgetError(503, "invalid persisted renewal schedule: %v", errSchedule)
				}
			}
			// Pending requests can straddle renewal or survive a crash. Carry their
			// whole bound into the new allowance until a proven settlement exists.
			s.UsedMicroUSD, s.CycleStart, s.NextRenewal = pending, start, next
		}
	}
	if reconcile {
		s.LimitMicroUSD = b.LimitMicroUSD
	}
	errMutate := mutate(&s)
	if err = writeClaudeBudgetState(b.StatePath, &s); err != nil {
		return budgetError(503, "cannot persist durable state: %v", err)
	}
	return errMutate
}

func writeClaudeBudgetState(path string, s *ClaudeBudgetState) (err error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".claude-budget-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	errClose := f.Close()
	if err != nil {
		return err
	}
	if errClose != nil {
		return errClose
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	errClose = dir.Close()
	if err != nil {
		return err
	}
	return errClose
}

// ReserveClaudeBudget commits the worst-case charge before any paid send.
func ReserveClaudeBudget(id string, b config.ClaudeAPIBudget, price config.ClaudeAPIPrice, maxOutput int64, now time.Time) (*ClaudeBudgetReservation, error) {
	inputRate := max(price.Input, price.CacheWrite5m, price.CacheWrite1h, price.CacheRead)
	input, err := claudeBudgetTokenCost(price.MaxInputTokens, inputRate)
	if err != nil {
		return nil, err
	}
	output, err := claudeBudgetTokenCost(maxOutput, price.Output)
	if err != nil {
		return nil, err
	}
	amount := input + output
	r := &ClaudeBudgetReservation{id: id, config: b, requestID: uuid.NewString(), price: price, maxOutput: maxOutput}
	err = budgetTransaction(id, b, now, true, func(s *ClaudeBudgetState) error {
		if s.BlockedReason != "" {
			return budgetError(503, "%s blocked pending reconciliation: %s", id, s.BlockedReason)
		}
		if amount <= 0 || s.UsedMicroUSD > b.LimitMicroUSD-amount {
			return budgetError(429, "%s blocked: used_micro_usd=%d limit_micro_usd=%d remaining_micro_usd=%d reserve_micro_usd=%d next_renewal=%s",
				id, s.UsedMicroUSD, b.LimitMicroUSD, max(int64(0), b.LimitMicroUSD-s.UsedMicroUSD), amount, s.NextRenewal.Format(time.RFC3339))
		}
		s.UsedMicroUSD += amount
		s.Pending[r.requestID] = amount
		return nil
	})
	return r, err
}

// Settle removes a reservation exactly once. Missing/invalid usage must never call
// this method. A cost exceeding its bound charges the excess and blocks new sends.
func (r *ClaudeBudgetReservation) Settle(amount int64, now time.Time) error {
	if r == nil {
		return nil
	}
	return budgetTransaction(r.id, r.config, now, false, func(s *ClaudeBudgetState) error {
		reserved, exists := s.Pending[r.requestID]
		if !exists {
			return nil
		}
		if amount < 0 || amount > math.MaxInt64-(s.UsedMicroUSD-reserved) {
			return budgetError(503, "invalid settlement")
		}
		s.UsedMicroUSD += amount - reserved
		delete(s.Pending, r.requestID)
		if amount > reserved {
			s.UsedMicroUSD = max(s.UsedMicroUSD, s.LimitMicroUSD)
			s.BlockedReason = "settlement exceeded the approved reservation"
		}
		return nil
	})
}

func (r *ClaudeBudgetReservation) blockUnpricedUsage(now time.Time) error {
	return budgetTransaction(r.id, r.config, now, false, func(s *ClaudeBudgetState) error {
		s.BlockedReason = "provider usage could not be priced within the approved reservation"
		return nil
	})
}

func claudeBudgetTokenCost(tokens, rate int64) (int64, error) {
	// Bounds keep multiplication and summation exact without floating point.
	if tokens < 0 || tokens > 10_000_000 || rate <= 0 || rate > 100_000_000_000 {
		return 0, budgetError(503, "invalid token count or configured price")
	}
	return (tokens*rate + 999_999) / 1_000_000, nil
}

func validateClaudeBudgetPrice(p config.ClaudeAPIPrice) error {
	if p.MaxInputTokens <= 0 || p.MaxOutputTokens <= 0 {
		return budgetError(503, "model context/output ceilings must be configured")
	}
	for _, rate := range []int64{p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead} {
		if _, err := claudeBudgetTokenCost(max(p.MaxInputTokens, p.MaxOutputTokens), rate); err != nil {
			return err
		}
	}
	return nil
}

func claudeBudgetAllowedBeta(b config.ClaudeAPIBudget, beta string) bool {
	for _, allowed := range b.AllowedBetas {
		if strings.TrimSpace(allowed) == beta {
			return true
		}
	}
	return false
}
