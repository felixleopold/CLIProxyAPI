// Package usageledger keeps a persistent per-client usage ledger: hourly UTC
// buckets of token counts keyed by client key fingerprint, provider and model.
// Cost is derived from token counts at query time, so price fixes apply
// retroactively. Raw API keys are never stored.
package usageledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultFileName = "usage-ledger.json"
	fileVersion     = 1
)

// flushInterval is the debounce between a record dirtying the ledger and the
// file write. It is a variable only so tests can shorten it.
var flushInterval = 30 * time.Second

// Fingerprint returns the first 12 hex chars of sha256(key); "" for no key.
func Fingerprint(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// Bucket holds the token counts of one (hour, client, provider, model) key.
// InputTokens excludes cache tokens. OutputTokens includes reasoning tokens,
// which ReasoningTokens reports as a subset, so output is billed once.
type Bucket struct {
	Hour             time.Time `json:"hour"`
	Fingerprint      string    `json:"fingerprint"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Requests         int64     `json:"requests"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
}

// PricedBucket is a Bucket with its query-time cost.
type PricedBucket struct {
	Bucket
	CostUSD float64 `json:"cost_usd"`
	Priced  bool    `json:"priced"`
}

type bucketKey struct {
	hour        int64
	fingerprint string
	provider    string
	model       string
}

// Ledger is a concurrency-safe, file-backed usage ledger.
type Ledger struct {
	// writeMu serializes file writes; it is always taken before mu.
	writeMu sync.Mutex
	mu      sync.Mutex
	enabled bool
	path    string
	buckets map[bucketKey]*Bucket
	dirty   bool
	timer   *time.Timer
	now     func() time.Time
}

// New returns a disabled ledger.
func New() *Ledger {
	return &Ledger{buckets: make(map[bucketKey]*Bucket), now: time.Now}
}

var defaultLedger = New()

// Default returns the process-wide ledger fed by the usage plugin.
func Default() *Ledger { return defaultLedger }

// Enabled reports whether the ledger is recording.
func (l *Ledger) Enabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// ResolvePath returns the configured ledger path, or usage-ledger.json next to
// the config file.
func ResolvePath(cfg config.UsageLedgerConfig, configFile string) string {
	if p := strings.TrimSpace(cfg.Path); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(configFile), defaultFileName)
}

// Configure applies the usage-ledger config. Enabling loads the file; disabling
// or moving the file flushes first. A failed flush or load keeps the current
// state so unsaved or existing history is never replaced; the next reload
// retries. Safe to call on every config reload.
func (l *Ledger) Configure(cfg *config.Config, configFile string) {
	if l == nil || cfg == nil {
		return
	}
	enable := cfg.UsageLedger.Enabled
	path := ResolvePath(cfg.UsageLedger, configFile)

	// Hold both locks for the whole transition so concurrent records wait
	// instead of being dropped, and no flush races the path change.
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.enabled == enable && (!enable || l.path == path) {
		return
	}
	if l.enabled && l.dirty {
		snapshot := sortedBuckets(l.buckets, time.Time{}, time.Time{})
		if err := writeFile(l.path, fileFormat{Version: fileVersion, Buckets: snapshot}); err != nil {
			log.Errorf("usage ledger: flush before reconfigure failed, keeping %s: %v", l.path, err)
			return
		}
		l.dirty = false
	}
	if !enable {
		l.enabled = false
		return
	}
	buckets, err := load(path)
	if err != nil {
		log.Errorf("usage ledger: not switching to %s: %v", path, err)
		return
	}
	l.path, l.buckets, l.dirty, l.enabled = path, buckets, false, true
	log.Infof("usage ledger: recording to %s", path)
}

// Shutdown flushes pending data and stops the debounce timer.
func (l *Ledger) Shutdown() {
	if l == nil {
		return
	}
	if err := l.Flush(); err != nil {
		log.Errorf("usage ledger: shutdown flush failed: %v", err)
	}
}

// Record adds one request. Failed requests count only when upstream reported
// usage, since a failure after generation is still billed.
func (l *Ledger) Record(rec coreusage.Record) {
	if l == nil {
		return
	}
	tokens := normalizeTokens(rec)
	if rec.Failed && tokens == (tokenCounts{}) {
		return
	}
	key := bucketKey{
		hour:        hourOf(rec.RequestedAt, l.now()).Unix(),
		fingerprint: Fingerprint(rec.APIKey),
		provider:    strings.ToLower(strings.TrimSpace(rec.Provider)),
		model:       recordModel(rec),
	}
	if key.provider == "" {
		key.provider = "unknown"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled {
		return
	}
	b := l.buckets[key]
	if b == nil {
		b = &Bucket{Hour: time.Unix(key.hour, 0).UTC(), Fingerprint: key.fingerprint, Provider: key.provider, Model: key.model}
		l.buckets[key] = b
	}
	b.Requests++
	b.InputTokens += tokens.input
	b.OutputTokens += tokens.output
	b.CacheReadTokens += tokens.cacheRead
	b.CacheWriteTokens += tokens.cacheWrite
	b.ReasoningTokens += tokens.reasoning
	l.dirty = true
	if l.timer == nil {
		l.timer = time.AfterFunc(flushInterval, func() {
			if err := l.Flush(); err != nil {
				log.Errorf("usage ledger: flush failed: %v", err)
			}
		})
	}
}

// recordModel picks the model that best matches what upstream billed: the
// model the response reports (an upstream may serve a different one), then the
// post-alias upstream model, then the alias the client asked for.
func recordModel(rec coreusage.Record) string {
	if m := strings.TrimSpace(rec.ResponseModel); m != "" {
		return m
	}
	if m := strings.TrimSpace(rec.Model); m != "" {
		return m
	}
	if m := strings.TrimSpace(rec.Alias); m != "" {
		return m
	}
	return "unknown"
}

func hourOf(t, fallback time.Time) time.Time {
	if t.IsZero() {
		t = fallback
	}
	return t.UTC().Truncate(time.Hour)
}

type tokenCounts struct {
	input, output, cacheRead, cacheWrite, reasoning int64
}

// normalizeTokens returns billable, non-overlapping counts: input excludes cache
// tokens and output includes reasoning. The provider-aware token breakdown owns
// the semantics (Claude reports cache separately; OpenAI/Codex input includes it).
func normalizeTokens(rec coreusage.Record) tokenCounts {
	d := coreusage.EnsureTokenBreakdownForProvider(rec.Detail, rec.Provider, rec.ExecutorType)
	if b := d.TokenBreakdown; b.Quality == coreusage.TokenAccountingQualityComplete {
		return tokenCounts{
			input:      b.Input.UncachedTokens,
			output:     b.Output.TotalTokens,
			cacheRead:  b.Input.CacheReadTokens,
			cacheWrite: b.Input.CacheWriteTokens,
			reasoning:  b.Output.ReasoningTokens,
		}
	}
	// Unclassified or inconsistent usage: use the raw fields, treating input as
	// cache-inclusive unless the provider is known to report cache separately.
	c := tokenCounts{input: d.InputTokens, output: d.OutputTokens, cacheRead: d.CacheReadTokens, cacheWrite: d.CacheCreationTokens, reasoning: d.ReasoningTokens}
	provider := strings.ToLower(rec.Provider + " " + rec.ExecutorType)
	if !strings.Contains(provider, "claude") && !strings.Contains(provider, "anthropic") {
		if cache := c.cacheRead + c.cacheWrite; cache <= c.input {
			c.input -= cache
		}
	}
	return c
}

type fileFormat struct {
	Version int      `json:"version"`
	Buckets []Bucket `json:"buckets"`
}

func load(path string) (map[bucketKey]*Bucket, error) {
	buckets := make(map[bucketKey]*Bucket)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return buckets, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	var f fileFormat
	if err = json.Unmarshal(data, &f); err != nil || f.Version != fileVersion {
		// Keep the unreadable file for inspection instead of overwriting it.
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if errRename := os.Rename(path, backup); errRename != nil {
			return nil, fmt.Errorf("ledger %s unreadable (version %d, %v) and could not be moved aside: %w", path, f.Version, err, errRename)
		}
		log.Errorf("usage ledger: %s unreadable (version %d, %v); moved to %s and starting empty", path, f.Version, err, backup)
		return buckets, nil
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		b.Hour = b.Hour.UTC().Truncate(time.Hour)
		buckets[bucketKey{b.Hour.Unix(), b.Fingerprint, b.Provider, b.Model}] = &b
	}
	return buckets, nil
}

// Flush writes the ledger atomically (temp file + rename, 0600) when dirty.
func (l *Ledger) Flush() error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	l.mu.Lock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if !l.dirty || l.path == "" {
		l.mu.Unlock()
		return nil
	}
	path := l.path
	snapshot := sortedBuckets(l.buckets, time.Time{}, time.Time{})
	l.dirty = false
	l.mu.Unlock()

	if err := writeFile(path, fileFormat{Version: fileVersion, Buckets: snapshot}); err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
		return err
	}
	return nil
}

func writeFile(path string, f fileFormat) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode ledger: %w", err)
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create ledger dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp ledger: %w", err)
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(data)
	if errWrite == nil {
		errWrite = tmp.Sync()
	}
	if errClose := tmp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Chmod(tmpName, 0o600)
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpName, path)
	}
	if errWrite != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write ledger: %w", errWrite)
	}
	return nil
}

// sortedBuckets copies buckets with from <= hour < to (zero bound = open).
func sortedBuckets(m map[bucketKey]*Bucket, from, to time.Time) []Bucket {
	out := make([]Bucket, 0, len(m))
	for _, b := range m {
		if (!from.IsZero() && b.Hour.Before(from.UTC().Truncate(time.Hour))) || (!to.IsZero() && !b.Hour.Before(to)) {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Hour.Equal(b.Hour) {
			return a.Hour.Before(b.Hour)
		}
		if a.Fingerprint != b.Fingerprint {
			return a.Fingerprint < b.Fingerprint
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return out
}

// QueryResult is a priced slice of the ledger.
type QueryResult struct {
	Buckets        []PricedBucket
	UnpricedModels []string
}

// Query returns buckets in [from, to) priced with prices (zero bound = open).
func (l *Ledger) Query(from, to time.Time, prices *PriceTable) QueryResult {
	l.mu.Lock()
	buckets := sortedBuckets(l.buckets, from, to)
	l.mu.Unlock()

	res := QueryResult{Buckets: make([]PricedBucket, 0, len(buckets)), UnpricedModels: []string{}}
	unpriced := make(map[string]bool)
	for _, b := range buckets {
		pb := PricedBucket{Bucket: b}
		if p, ok := prices.Lookup(b.Model); ok {
			pb.Priced = true
			pb.CostUSD = p.Cost(b.InputTokens, b.OutputTokens, b.CacheReadTokens, b.CacheWriteTokens)
		} else {
			unpriced[b.Model] = true
		}
		res.Buckets = append(res.Buckets, pb)
	}
	for m := range unpriced {
		res.UnpricedModels = append(res.UnpricedModels, m)
	}
	sort.Strings(res.UnpricedModels)
	return res
}

// Client describes a client key fingerprint with its optional labels.
type Client struct {
	Fingerprint string `json:"fingerprint"`
	Device      string `json:"device"`
	User        string `json:"user"`
	Configured  bool   `json:"configured"`
}

// BuildClients unions every configured api key fingerprint (configured=true),
// the labelled entries and the fingerprints seen in the ledger.
func BuildClients(labels []config.UsageLedgerClient, apiKeys []string, seen []string) []Client {
	byFP := make(map[string]*Client)
	get := func(fp string) *Client {
		if c := byFP[fp]; c != nil {
			return c
		}
		c := &Client{Fingerprint: fp}
		byFP[fp] = c
		return c
	}
	for _, key := range apiKeys {
		if fp := Fingerprint(key); fp != "" {
			get(fp).Configured = true
		}
	}
	for _, label := range labels {
		fp := strings.TrimSpace(label.Fingerprint)
		if fp == "" {
			continue
		}
		c := get(fp)
		c.Device, c.User = label.Device, label.User
	}
	for _, fp := range seen {
		get(fp)
	}
	out := make([]Client, 0, len(byFP))
	for _, c := range byFP {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// SeenFingerprints lists the distinct fingerprints in buckets.
func SeenFingerprints(buckets []PricedBucket) []string {
	set := make(map[string]bool)
	var out []string
	for _, b := range buckets {
		if !set[b.Fingerprint] {
			set[b.Fingerprint] = true
			out = append(out, b.Fingerprint)
		}
	}
	return out
}
