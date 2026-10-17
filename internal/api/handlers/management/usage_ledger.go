package management

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageledger"
)

const usageLedgerDefaultWindow = 35 * 24 * time.Hour

var usageLedgerFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// usageLedgerState snapshots the ledger config and client keys, replying 409
// when the ledger is disabled.
func (h *Handler) usageLedgerState(c *gin.Context) (config.UsageLedgerConfig, []string, bool) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return config.UsageLedgerConfig{}, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return config.UsageLedgerConfig{}, nil, false
	}
	cfg := h.cfg.UsageLedger
	if !cfg.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "usage ledger disabled"})
		return config.UsageLedgerConfig{}, nil, false
	}
	cfg.Clients = append([]config.UsageLedgerClient(nil), cfg.Clients...)
	cfg.Prices = append([]config.UsageLedgerPrice(nil), cfg.Prices...)
	return cfg, append([]string(nil), h.cfg.APIKeys...), true
}

// GetUsageLedger returns priced hourly buckets within [from, to).
func (h *Handler) GetUsageLedger(c *gin.Context) {
	cfg, apiKeys, ok := h.usageLedgerState(c)
	if !ok {
		return
	}
	now := time.Now().UTC()
	from, to := now.Add(-usageLedgerDefaultWindow), now
	if v := strings.TrimSpace(c.Query("from")); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "from must be RFC3339"})
			return
		}
		from = parsed.UTC()
	}
	if v := strings.TrimSpace(c.Query("to")); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "to must be RFC3339"})
			return
		}
		to = parsed.UTC()
	}
	if !from.Before(to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must be before to"})
		return
	}

	result := usageledger.Default().Query(from, to, usageledger.NewPriceTable(cfg.Prices))
	c.JSON(http.StatusOK, gin.H{
		"generated_at":    now.Format(time.RFC3339),
		"from":            from.Format(time.RFC3339),
		"to":              to.Format(time.RFC3339),
		"buckets":         result.Buckets,
		"clients":         usageledger.BuildClients(cfg.Clients, apiKeys, usageledger.SeenFingerprints(result.Buckets)),
		"unpriced_models": result.UnpricedModels,
	})
}

// GetUsageLedgerClients returns every known client fingerprint with its labels.
func (h *Handler) GetUsageLedgerClients(c *gin.Context) {
	cfg, apiKeys, ok := h.usageLedgerState(c)
	if !ok {
		return
	}
	all := usageledger.Default().Query(time.Time{}, time.Time{}, usageledger.NewPriceTable(nil))
	c.JSON(http.StatusOK, gin.H{"clients": usageledger.BuildClients(cfg.Clients, apiKeys, usageledger.SeenFingerprints(all.Buckets))})
}

// PutUsageLedgerClients replaces the client label list and persists it to the config file.
func (h *Handler) PutUsageLedgerClients(c *gin.Context) {
	if _, _, ok := h.usageLedgerState(c); !ok {
		return
	}
	var body struct {
		Clients []config.UsageLedgerClient `json:"clients"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	seen := make(map[string]bool, len(body.Clients))
	labels := make([]config.UsageLedgerClient, 0, len(body.Clients))
	for _, client := range body.Clients {
		client.Fingerprint = strings.ToLower(strings.TrimSpace(client.Fingerprint))
		client.Device, client.User = strings.TrimSpace(client.Device), strings.TrimSpace(client.User)
		if !usageLedgerFingerprintPattern.MatchString(client.Fingerprint) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "fingerprint must be 12 hex characters", "fingerprint": client.Fingerprint})
			return
		}
		if seen[client.Fingerprint] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate fingerprint", "fingerprint": client.Fingerprint})
			return
		}
		seen[client.Fingerprint] = true
		labels = append(labels, client)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.cfg.UsageLedger.Clients
	h.cfg.UsageLedger.Clients = labels
	snapshot, saved := h.saveConfigAndSnapshotLocked(c)
	if !saved {
		h.cfg.UsageLedger.Clients = previous
		return
	}
	apiKeys := append([]string(nil), h.cfg.APIKeys...)
	all := usageledger.Default().Query(time.Time{}, time.Time{}, usageledger.NewPriceTable(nil))
	c.JSON(http.StatusOK, gin.H{"clients": usageledger.BuildClients(labels, apiKeys, usageledger.SeenFingerprints(all.Buckets))})
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
}

// GetUsageLedgerPrices returns the effective price table (USD per 1M tokens).
func (h *Handler) GetUsageLedgerPrices(c *gin.Context) {
	cfg, _, ok := h.usageLedgerState(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"prices": usageledger.NewPriceTable(cfg.Prices).Entries()})
}
