package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageledger"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func getUsageLedger(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/ledger"+query, nil)
	h.GetUsageLedger(ctx)
	return rec
}

func TestGetUsageLedgerFiltersByRangeAndReportsClients(t *testing.T) {
	cfg := &config.Config{UsageLedger: config.UsageLedgerConfig{
		Enabled: true,
		Path:    filepath.Join(t.TempDir(), "ledger.json"),
		Clients: []config.UsageLedgerClient{{Fingerprint: usageledger.Fingerprint("mac-key"), Device: "Mac", User: "Felix"}},
	}}
	cfg.APIKeys = []string{"mac-key", "idle-key"}
	h := &Handler{cfg: cfg}

	ledger := usageledger.Default()
	ledger.Configure(cfg, "unused.yaml")
	t.Cleanup(func() { ledger.Configure(&config.Config{}, "unused.yaml") })

	early := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	record := func(model string, at time.Time) coreusage.Record {
		return coreusage.Record{
			Provider: "claude", ExecutorType: "ClaudeExecutor", Model: model, APIKey: "mac-key", RequestedAt: at,
			Detail: coreusage.Detail{InputTokens: 1_000_000, OutputTokens: 1_000_000, TokenBreakdown: coreusage.NewIndependentTokenBreakdown(1_000_000, 0, 0, 1_000_000, 0, 0)},
		}
	}
	ledger.Record(record("claude-opus-5-5", early))
	ledger.Record(record("claude-opus-5-5", late))
	ledger.Record(record("claude-3-7-sonnet-20250219", late))

	rec := getUsageLedger(t, h, "?from=2026-10-12T00:00:00Z&to=2026-10-14T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Buckets []struct {
			Hour    time.Time `json:"hour"`
			Model   string    `json:"model"`
			CostUSD float64   `json:"cost_usd"`
			Priced  bool      `json:"priced"`
		} `json:"buckets"`
		Clients        []usageledger.Client `json:"clients"`
		UnpricedModels []string             `json:"unpriced_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want only the two inside the range", body.Buckets)
	}
	for _, b := range body.Buckets {
		if !b.Hour.Equal(late) {
			t.Errorf("bucket hour = %v, want %v", b.Hour, late)
		}
		switch b.Model {
		case "claude-opus-5-5":
			if !b.Priced || b.CostUSD != 24 { // 1M input at $4 + 1M output at $20
				t.Errorf("opus bucket = %+v, want priced at $24", b)
			}
		default:
			if b.Priced || b.CostUSD != 0 {
				t.Errorf("unpriced bucket = %+v", b)
			}
		}
	}
	if len(body.UnpricedModels) != 1 || body.UnpricedModels[0] != "claude-3-7-sonnet-20250219" {
		t.Errorf("unpriced_models = %v", body.UnpricedModels)
	}
	// Both configured keys are listed; the labelled one carries its labels.
	if len(body.Clients) != 2 {
		t.Fatalf("clients = %+v", body.Clients)
	}
	for _, c := range body.Clients {
		wantLabel := c.Fingerprint == usageledger.Fingerprint("mac-key")
		if !c.Configured || (c.Device == "Mac") != wantLabel {
			t.Errorf("client = %+v", c)
		}
	}

	if rec = getUsageLedger(t, h, "?from=nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad from: status = %d", rec.Code)
	}

	cfg.UsageLedger.Enabled = false
	if rec = getUsageLedger(t, h, ""); rec.Code != http.StatusConflict {
		t.Errorf("disabled: status = %d, want 409", rec.Code)
	}
}
