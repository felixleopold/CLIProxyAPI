package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DefaultClaudeBudgetTier runs before user payload rules. Standard-only capacity
// makes the token rates explicit without breaking ordinary SDK requests that omit it.
func DefaultClaudeBudgetTier(cfg *config.Config, auth *cliproxyauth.Auth, payload []byte) ([]byte, error) {
	id, _, err := ClaudeBudgetForAuth(cfg, auth)
	if err != nil {
		return nil, err
	}
	if id == "" || gjson.GetBytes(payload, "service_tier").Exists() {
		return payload, nil
	}
	return sjson.SetBytes(payload, "service_tier", "standard_only")
}

// ClaudeBudgetRequestPrice validates the final payload read-only. Unpriced
// features are rejected rather than rewritten after user payload configuration.
func ClaudeBudgetRequestPrice(b config.ClaudeAPIBudget, payload []byte, betas string) (config.ClaudeAPIPrice, int64, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil || root == nil {
		return config.ClaudeAPIPrice{}, 0, budgetError(400, "invalid inference payload")
	}
	if err := rejectClaudeBudgetDuplicateKeys(payload); err != nil {
		return config.ClaudeAPIPrice{}, 0, err
	}
	allowedFields := map[string]bool{
		"model": true, "messages": true, "system": true, "max_tokens": true, "stream": true,
		"metadata": true, "temperature": true, "top_p": true, "top_k": true, "stop_sequences": true,
		"tools": true, "tool_choice": true, "thinking": true, "output_config": true,
		"cache_control": true, "context_management": true, "speed": true, "service_tier": true, "inference_geo": true,
	}
	for key := range root {
		if !allowedFields[key] {
			return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced request feature %q", key)
		}
	}
	r := gjson.ParseBytes(payload)
	if r.Get("service_tier").String() != "standard_only" {
		return config.ClaudeAPIPrice{}, 0, budgetError(400, "protected requests require standard_only service_tier")
	}
	if err := validateClaudeBudgetCacheControls(r); err != nil {
		return config.ClaudeAPIPrice{}, 0, err
	}
	for _, field := range []struct{ name, accepted string }{{"speed", "standard"}, {"service_tier", "standard_only"}, {"inference_geo", "global"}} {
		if v := r.Get(field.name); v.Exists() && v.String() != field.accepted {
			return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced %s", field.name)
		}
	}
	for _, beta := range strings.Split(betas, ",") {
		beta = strings.TrimSpace(beta)
		for _, unpriced := range []string{"fast-mode-", "compact-", "compaction-", "advisor-tool-", "server-side-fallback-", "fallback-credit-"} {
			if strings.HasPrefix(beta, unpriced) {
				return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced billing beta %q", beta)
			}
		}
		if beta != "" && !claudeBudgetAllowedBeta(b, beta) {
			return config.ClaudeAPIPrice{}, 0, budgetError(400, "unapproved billing beta %q", beta)
		}
	}
	for _, tool := range r.Get("tools").Array() {
		if err := validateClaudeBudgetTool(tool); err != nil {
			return config.ClaudeAPIPrice{}, 0, err
		}
	}
	for _, message := range r.Get("messages").Array() {
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() != "tool_addition" {
				continue
			}
			switch block.Get("tool.type").String() {
			case "tool_reference": // References use definitions validated in tools[].
			case "tool_definition":
				if err := validateClaudeBudgetTool(block.Get("tool.definition")); err != nil {
					return config.ClaudeAPIPrice{}, 0, err
				}
			default:
				return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced inline tool kind")
			}
		}
	}
	for _, edit := range r.Get("context_management.edits").Array() {
		switch edit.Get("type").String() {
		case "clear_tool_uses_20250919", "clear_thinking_20251015":
		default:
			return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced context-management sampling")
		}
	}
	for key := range r.Get("output_config").Map() {
		if key != "effort" && key != "format" {
			return config.ClaudeAPIPrice{}, 0, budgetError(400, "unpriced output_config feature %q", key)
		}
	}
	price, exists := b.Models[r.Get("model").String()]
	if !exists {
		return price, 0, budgetError(400, "model has no approved budget prices")
	}
	if err := validateClaudeBudgetPrice(price); err != nil {
		return price, 0, err
	}
	var maxOutput int64
	if err := json.Unmarshal(root["max_tokens"], &maxOutput); err != nil || maxOutput <= 0 || maxOutput > price.MaxOutputTokens {
		return price, 0, budgetError(400, "max_tokens must be a positive integer within the approved output ceiling")
	}
	return price, maxOutput, nil
}

// Inline definitions and the initial tool list share the same billing rules.
func validateClaudeBudgetTool(tool gjson.Result) error {
	if !tool.IsObject() {
		return budgetError(400, "invalid tool definition")
	}
	kind := tool.Get("type").String()
	switch kind {
	case "", "custom", "bash_20250124", "text_editor_20250124", "text_editor_20250429", "text_editor_20250728", "computer_20250124", "computer_20251124":
		return nil
	default:
		return budgetError(400, "unpriced server/tool type %q", kind)
	}
}

func rejectClaudeBudgetDuplicateKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return budgetError(400, "invalid inference JSON")
		}
		switch token {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return budgetError(400, "invalid inference JSON")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return budgetError(400, "duplicate inference JSON field")
				}
				seen[name] = true
				if err = visit(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		case json.Delim('['):
			for decoder.More() {
				if err = visit(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		}
		return err
	}
	return visit()
}

func validateClaudeBudgetCacheControls(node gjson.Result) error {
	if cache := node.Get("cache_control"); cache.Exists() {
		if !cache.IsObject() || cache.Get("type").String() != "ephemeral" {
			return budgetError(400, "unpriced cache control")
		}
		if ttl := cache.Get("ttl"); ttl.Exists() && ttl.String() != "5m" && ttl.String() != "1h" {
			return budgetError(400, "unpriced cache duration")
		}
	}
	// Only inspect protocol content, never user tool input or JSON schemas.
	for _, field := range []string{"messages", "system", "tools", "content"} {
		for _, child := range node.Get(field).Array() {
			if err := validateClaudeBudgetCacheControls(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ClaudeBudgetReservation) usageCost(raw []byte) (int64, error) {
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(raw, &usage); err != nil || usage == nil {
		return 0, fmt.Errorf("missing usage")
	}
	count := func(key string, required bool) (int64, error) {
		data, exists := usage[key]
		if !exists && !required {
			return 0, nil
		}
		var value int64
		if !exists || string(data) == "null" || json.Unmarshal(data, &value) != nil || value < 0 || value > 10_000_000 {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return value, nil
	}
	input, err := count("input_tokens", true)
	if err != nil {
		return 0, err
	}
	output, err := count("output_tokens", true)
	if err != nil {
		return 0, err
	}
	read, err := count("cache_read_input_tokens", false)
	if err != nil {
		return 0, err
	}
	creation, err := count("cache_creation_input_tokens", false)
	if err != nil {
		return 0, err
	}
	// Ignore informational thinking counts: they are already in output_tokens.
	for key, value := range usage {
		switch key {
		case "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "thinking_tokens", "service_tier", "inference_geo":
		case "output_tokens_details":
			var details map[string]int64
			if json.Unmarshal(value, &details) != nil {
				return 0, fmt.Errorf("invalid output token details")
			}
			for category, n := range details {
				if category != "thinking_tokens" || n < 0 || n > output {
					return 0, fmt.Errorf("unpriced output token details")
				}
			}
		case "server_tool_use":
			var tools map[string]int64
			if json.Unmarshal(value, &tools) != nil {
				return 0, fmt.Errorf("invalid server tool usage")
			}
			for _, n := range tools {
				if n != 0 {
					return 0, fmt.Errorf("unpriced server usage")
				}
			}
		default:
			return 0, fmt.Errorf("unknown usage category %s", key)
		}
	}
	for _, field := range []struct{ name, accepted string }{{"service_tier", "standard"}, {"inference_geo", "global"}} {
		if v, exists := usage[field.name]; exists && string(v) != "null" && strings.Trim(string(v), "\"") != field.accepted {
			return 0, fmt.Errorf("unpriced response %s", field.name)
		}
	}
	write5, write1 := int64(0), int64(0)
	if detail, exists := usage["cache_creation"]; exists && string(detail) != "null" {
		var cache map[string]int64
		if json.Unmarshal(detail, &cache) != nil {
			return 0, fmt.Errorf("invalid cache breakdown")
		}
		for key, n := range cache {
			if n < 0 || (key != "ephemeral_5m_input_tokens" && key != "ephemeral_1h_input_tokens") {
				return 0, fmt.Errorf("unpriced cache duration")
			}
		}
		write5, write1 = cache["ephemeral_5m_input_tokens"], cache["ephemeral_1h_input_tokens"]
		if write5+write1 != creation {
			return 0, fmt.Errorf("inconsistent cache breakdown")
		}
	} else {
		// Old responses without a TTL breakdown retain the higher cache-write price.
		write1 = creation
	}
	if input+read+creation > r.price.MaxInputTokens || output > r.maxOutput {
		return 0, fmt.Errorf("usage exceeds the approved reservation bound")
	}
	var total int64
	for _, item := range []struct{ n, rate int64 }{
		{input, r.price.Input}, {output, r.price.Output}, {read, r.price.CacheRead},
		{write5, r.price.CacheWrite5m}, {write1, max(r.price.CacheWrite1h, r.price.CacheWrite5m)},
	} {
		cost, err := claudeBudgetTokenCost(item.n, item.rate)
		if err != nil {
			return 0, err
		}
		total += cost
	}
	return total, nil
}

// ObserveClaudeBudgetBody observes decompressed, untranslated provider JSON/SSE.
// It settles synchronously before exposing message_stop or successful JSON EOF.
// Cancellation, malformed usage, errors and incomplete streams keep reservations.
func ObserveClaudeBudgetBody(body io.ReadCloser, reservation *ClaudeBudgetReservation, stream bool) io.ReadCloser {
	if reservation == nil {
		return body
	}
	return &claudeBudgetBody{ReadCloser: body, reservation: reservation, stream: stream}
}

type claudeBudgetBody struct {
	io.ReadCloser
	reservation *ClaudeBudgetReservation
	stream      bool
	buffer      []byte
	usage       map[string]json.RawMessage
	started     bool
	delta       bool
	complete    bool
	failed      bool
}

func (b *claudeBudgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.complete || b.failed {
		return n, err
	}
	b.buffer = append(b.buffer, p[:n]...)
	if len(b.buffer) > 64*1024*1024 {
		b.failed = true
		b.buffer = nil
		return n, err
	}
	if b.stream {
		for {
			end := bytes.IndexByte(b.buffer, '\n')
			if end < 0 {
				break
			}
			line := b.buffer[:end]
			if errObserve := b.observeLine(line); errObserve != nil {
				return 0, errObserve
			}
			b.buffer = b.buffer[end+1:]
		}
		if err == io.EOF && len(b.buffer) != 0 {
			if errObserve := b.observeLine(b.buffer); errObserve != nil {
				return 0, errObserve
			}
			b.buffer = nil
		}
	} else if err == io.EOF {
		root := gjson.ParseBytes(b.buffer)
		if gjson.ValidBytes(b.buffer) && root.Get("type").String() == "message" && root.Get("stop_reason").Type == gjson.String {
			cost, errCost := b.reservation.usageCost([]byte(root.Get("usage").Raw))
			if errCost == nil {
				if errSettle := b.reservation.Settle(cost, time.Now()); errSettle != nil {
					return 0, errSettle
				}
				b.complete = true
			} else if errBlock := b.reservation.blockUnpricedUsage(time.Now()); errBlock != nil {
				return 0, errBlock
			}
		}
		b.buffer = nil
	}
	if err != nil && err != io.EOF {
		b.failed = true
	}
	return n, err
}

func (b *claudeBudgetBody) observeLine(line []byte) error {
	if b.complete || b.failed {
		return nil
	}
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if !gjson.ValidBytes(payload) {
		b.failed = true
		return nil
	}
	root := gjson.ParseBytes(payload)
	var raw string
	switch root.Get("type").String() {
	case "message_start":
		if b.started {
			b.failed = true
			return nil
		}
		b.started = true
		raw = root.Get("message.usage").Raw
	case "message_delta":
		b.delta = root.Get("usage.output_tokens").Exists() && root.Get("delta.stop_reason").Type == gjson.String
		raw = root.Get("usage").Raw
	case "error":
		b.failed = true
	case "message_stop":
		if !b.started || !b.delta {
			return nil
		}
		usage, err := json.Marshal(b.usage)
		if err != nil {
			b.failed = true
			return nil
		}
		cost, err := b.reservation.usageCost(usage)
		if err != nil {
			b.failed = true
			return b.reservation.blockUnpricedUsage(time.Now())
		}
		if err = b.reservation.Settle(cost, time.Now()); err != nil {
			b.failed = true
			return err
		}
		b.complete = true
	}
	if raw != "" {
		var update map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &update) != nil {
			b.failed = true
			return nil
		}
		if b.usage == nil {
			b.usage = make(map[string]json.RawMessage)
		}
		for key, value := range update {
			b.usage[key] = value
		}
	}
	return nil
}
