package helps

import (
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ClaudeBudgetPolicy prevents an executor from admitting with superseded settings
// while a request is still being translated during ordinary configuration reload.
type ClaudeBudgetPolicy struct {
	entry       *claudeBudgetPolicyEntry
	fingerprint [32]byte
}

type claudeBudgetPolicyEntry struct {
	sync.Mutex
	fingerprint [32]byte
}

var claudeBudgetPolicies = struct {
	sync.Mutex
	paths map[string]*claudeBudgetPolicyEntry
}{paths: make(map[string]*claudeBudgetPolicyEntry)}

func claudeBudgetPolicyFingerprint(id string, b config.ClaudeAPIBudget) [32]byte {
	data, _ := json.Marshal(b)
	return sha256.Sum256(append([]byte(id+"\x00"), data...))
}

// RegisterClaudeBudgetPolicies runs when the service replaces its Claude executor.
// Identical settings share a policy, including across per-credential executors.
func RegisterClaudeBudgetPolicies(cfg *config.Config) map[string]ClaudeBudgetPolicy {
	if cfg == nil || len(cfg.Claude.APIBudgets) == 0 {
		return nil
	}
	policies := make(map[string]ClaudeBudgetPolicy, len(cfg.Claude.APIBudgets))
	claudeBudgetPolicies.Lock()
	defer claudeBudgetPolicies.Unlock()
	for id, b := range cfg.Claude.APIBudgets {
		path := filepath.Clean(b.StatePath)
		entry := claudeBudgetPolicies.paths[path]
		if entry == nil {
			entry = &claudeBudgetPolicyEntry{}
			claudeBudgetPolicies.paths[path] = entry
		}
		fingerprint := claudeBudgetPolicyFingerprint(id, b)
		entry.Lock()
		entry.fingerprint = fingerprint
		entry.Unlock()
		policies[id] = ClaudeBudgetPolicy{entry: entry, fingerprint: fingerprint}
	}
	return policies
}

// ReserveClaudeBudgetRequest keeps policy validation and durable admission in one
// short critical section. No upstream network operation holds this lock.
func ReserveClaudeBudgetRequest(policy ClaudeBudgetPolicy, id string, b config.ClaudeAPIBudget, payload []byte, betas string, now time.Time) (*ClaudeBudgetReservation, error) {
	if policy.entry == nil {
		return nil, budgetError(503, "protected executor has no registered budget policy")
	}
	policy.entry.Lock()
	defer policy.entry.Unlock()
	if policy.fingerprint != policy.entry.fingerprint || policy.fingerprint != claudeBudgetPolicyFingerprint(id, b) {
		return nil, budgetError(503, "budget settings changed while preparing request; retry with the current executor")
	}
	price, maxOutput, err := ClaudeBudgetRequestPrice(b, payload, betas)
	if err != nil {
		return nil, err
	}
	return ReserveClaudeBudget(id, b, price, maxOutput, now)
}
