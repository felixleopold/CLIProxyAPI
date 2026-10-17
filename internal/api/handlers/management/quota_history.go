package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

const quotaHistoryDefaultWindow = 14 * 24 * time.Hour

// GetQuotaHistory returns quota usage samples within [from, to), oldest first.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	enabled := cfg != nil && cfg.QuotaHistory.Enabled
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if !enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "quota history disabled"})
		return
	}

	now := time.Now().UTC()
	from, to := now.Add(-quotaHistoryDefaultWindow), now
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
	c.JSON(http.StatusOK, gin.H{"samples": quotahistory.Default().Query(from, to)})
}
