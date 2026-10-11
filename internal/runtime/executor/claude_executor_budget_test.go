//go:build unix

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func executorBudgetFixture(t *testing.T) (*ClaudeExecutor, *cliproxyauth.Auth, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.json")
	price := config.ClaudeAPIPrice{MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000,
		Input: 4_000_000, Output: 20_000_000, CacheWrite5m: 5_000_000, CacheWrite1h: 8_000_000, CacheRead: 200_000}
	cfg := &config.Config{DisableClaudeCloakMode: true, Claude: config.ClaudeConfig{
		APIBudgets: map[string]config.ClaudeAPIBudget{"shared": {StatePath: path, LimitMicroUSD: 100_000_000,
			InitialMicroUSD: 10_000, MonthlyAnchor: "2020-01-01T00:00:00", Models: map[string]config.ClaudeAPIPrice{"claude-opus-5-5": price},
			AllowedBetas: []string{"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"}}}},
		ClaudeKey: []config.ClaudeKey{{APIKey: "fake-api-key", BudgetID: "shared"}}}
	auth := &cliproxyauth.Auth{ID: "selected", Provider: "claude", Attributes: map[string]string{"api_key": "fake-api-key", "claude_budget_id": "shared"}}
	return NewClaudeExecutor(cfg), auth, path
}

func executorBudgetState(t *testing.T, path string) helps.ClaudeBudgetState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s helps.ClaudeBudgetState
	if err = json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaudeBudgetReserveBeforeEveryOutboundAttempt(t *testing.T) {
	e, auth, path := executorBudgetFixture(t)
	calls := 0
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.GetBody != nil {
			t.Fatal("protected POST can be replayed internally")
		}
		s := executorBudgetState(t, path)
		if len(s.Pending) != calls || s.UsedMicroUSD != 10_000+int64(calls)*8_000_200 {
			t.Fatalf("send preceded reservation: %#v", s)
		}
		return nil, errors.New("unknown upstream outcome")
	})}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-5-5","max_tokens":10,"service_tier":"standard_only","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = e.doClaudeUpstreamRequest(client, req, auth); err == nil {
			t.Fatal("missing send failure")
		}
	}
	b := e.cfg.Claude.APIBudgets["shared"]
	b.LimitMicroUSD = 10_000 + 2*8_000_200
	e.cfg.Claude.APIBudgets["shared"] = b
	e = NewClaudeExecutor(e.cfg)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-5-5","max_tokens":10,"service_tier":"standard_only","messages":[]}`))
	_, _, err := e.doClaudeUpstreamRequest(client, req, auth)
	if err == nil || calls != 2 {
		t.Fatalf("exhausted budget sent upstream: calls=%d err=%v", calls, err)
	}
}

func TestClaudeBudgetExecuteAndStreamSettleRawUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			e, auth, path := executorBudgetFixture(t)
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if len(executorBudgetState(t, path).Pending) != 1 {
					t.Fatal("no durable reservation at send")
				}
				payload := `{"type":"message","id":"msg-budget","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":2}}`
				if stream {
					payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-budget\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			if stream {
				resp, err := e.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range resp.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := e.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			s := executorBudgetState(t, path)
			if s.UsedMicroUSD != 10_440 || len(s.Pending) != 0 {
				t.Fatalf("raw response was not settled: %#v", s)
			}
		})
	}
}

func TestClaudeBudgetGenericHTTPGenerationCannotBypass(t *testing.T) {
	e, auth, path := executorBudgetFixture(t)
	calls := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"input_tokens":100}`)), Request: req}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	for _, endpoint := range []string{"/v1/messages", "/v1/messages/batches", "/v1/complete"} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com"+endpoint, strings.NewReader(`{}`))
		if _, err := e.HttpRequest(ctx, auth, req); err == nil {
			t.Fatalf("generic HTTP paid request bypassed: %s", endpoint)
		}
	}
	if calls != 0 {
		t.Fatal("blocked request reached upstream")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens", strings.NewReader(`{}`))
	resp, err := e.HttpRequest(ctx, auth, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("free count_tokens was blocked")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("count_tokens consumed an inference reservation")
	}
}

func TestClaudeBudgetOAuthSendBypasses(t *testing.T) {
	e, _, path := executorBudgetFixture(t)
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "fake-api-key", "auth_kind": "oauth"}}
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{}`))
	resp, reservation, err := e.doClaudeUpstreamRequest(client, req, auth)
	if err != nil || reservation != nil {
		t.Fatalf("subscription was budgeted: %v", err)
	}
	if err = resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("subscription touched paid state")
	}
}

func TestClaudeBudgetUsesFinalConfiguredPayload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e, auth, path := executorBudgetFixture(t)
		e.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "claude-opus-5-5"}}, Params: map[string]any{"max_tokens": 11}}}
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			payload, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(payload, "max_tokens").Int() != 11 {
				t.Fatal("budget guard overwrote configured output")
			}
			if executorBudgetState(t, path).UsedMicroUSD != 10_000+8_000_220 {
				t.Fatal("reserved before final payload rules")
			}
			return nil, errors.New("test stops after checking final send")
		})
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
		req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
		var err error
		if stream {
			_, err = e.ExecuteStream(ctx, auth, req, opts)
		} else {
			_, err = e.Execute(ctx, auth, req, opts)
		}
		if err == nil {
			t.Fatal("test send should fail")
		}
	}
}

func TestClaudeBudgetStaleExecutorCannotRestorePreviousCap(t *testing.T) {
	old, auth, path := executorBudgetFixture(t)
	cfg := *old.cfg
	b := cfg.Claude.APIBudgets["shared"]
	b.LimitMicroUSD = 1
	cfg.Claude.APIBudgets = map[string]config.ClaudeAPIBudget{"shared": b}
	current := NewClaudeExecutor(&cfg)
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("stale or insufficient allowance reached upstream")
		return nil, nil
	})}
	for _, e := range []*ClaudeExecutor{current, old} {
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-5-5","max_tokens":10,"service_tier":"standard_only","messages":[]}`))
		if _, _, err := e.doClaudeUpstreamRequest(client, req, auth); err == nil {
			t.Fatal("stale/new insufficient budget accepted")
		}
	}
	if executorBudgetState(t, path).LimitMicroUSD != 1 {
		t.Fatal("stale executor restored old cap")
	}
}

func TestClaudeBudgetPayloadFilterCannotRestoreImplicitAutoTier(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e, auth, _ := executorBudgetFixture(t)
		e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "claude-opus-5-5"}}, Params: []string{"service_tier"}}}
		transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("filtered standard-only tier reached upstream")
			return nil, nil
		})
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
		req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
		var err error
		if stream {
			_, err = e.ExecuteStream(ctx, auth, req, opts)
		} else {
			_, err = e.Execute(ctx, auth, req, opts)
		}
		if err == nil || !strings.Contains(err.Error(), "service_tier") {
			t.Fatalf("filtered tier restored or wrong failure: %v", err)
		}
	}
}

func TestClaudeBudgetTokenCountRedirectCannotGenerate(t *testing.T) {
	e, auth, path := executorBudgetFixture(t)
	calls := 0
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/v1/messages/count_tokens" {
			t.Fatal("count_tokens redirected into paid Messages")
		}
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://api.anthropic.com/v1/messages"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens", bytes.NewBufferString(`{}`))
	resp, reservation, err := e.doClaudeUpstreamRequest(client, req, auth)
	if err != nil || reservation != nil || calls != 1 {
		t.Fatalf("count redirect protection failed: calls=%d err=%v", calls, err)
	}
	if err = resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("count request consumed budget")
	}
}
