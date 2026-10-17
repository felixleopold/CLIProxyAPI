// Package quotahistory keeps a persistent history of how much of each
// credential's subscription quota windows is used. Samples come from the
// passive quota snapshot the auth manager already keeps per credential, so
// recording never issues an upstream request.
package quotahistory

import (
	"context"
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
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultFileName = "quota-history.json"
	fileVersion     = 1
	retention       = 35 * 24 * time.Hour
	sampleInterval  = 5 * time.Minute
)

// History is a concurrency-safe, file-backed quota history.
type History struct {
	// writeMu serializes file writes; it is always taken before mu.
	writeMu sync.Mutex
	mu      sync.Mutex
	enabled bool
	path    string
	samples []Sample
	// lastObserved is the newest snapshot time already sampled per credential.
	lastObserved map[string]time.Time
	dirty        bool
	now          func() time.Time
}

// New returns a disabled history.
func New() *History {
	return &History{lastObserved: make(map[string]time.Time), now: time.Now}
}

var defaultHistory = New()

// Default returns the process-wide quota history.
func Default() *History { return defaultHistory }

// Enabled reports whether the history is recording.
func (h *History) Enabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled
}

// ResolvePath returns the configured history path, or quota-history.json next
// to the config file.
func ResolvePath(cfg config.QuotaHistoryConfig, configFile string) string {
	if p := strings.TrimSpace(cfg.Path); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(configFile), defaultFileName)
}

// Configure applies the quota-history config. Enabling loads the file; disabling
// or moving the file flushes first. A failed flush or load keeps the current
// state so unsaved or existing history is never replaced; the next reload
// retries. Safe to call on every config reload.
func (h *History) Configure(cfg *config.Config, configFile string) {
	if h == nil || cfg == nil {
		return
	}
	enable := cfg.QuotaHistory.Enabled
	path := ResolvePath(cfg.QuotaHistory, configFile)

	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.enabled == enable && (!enable || h.path == path) {
		return
	}
	if h.enabled && h.dirty {
		if err := writeFile(h.path, fileFormat{Version: fileVersion, Samples: h.sortedSamples(time.Time{}, time.Time{})}); err != nil {
			log.Errorf("quota history: flush before reconfigure failed, keeping %s: %v", h.path, err)
			return
		}
		h.dirty = false
	}
	if !enable {
		h.enabled = false
		return
	}
	samples, err := load(path)
	if err != nil {
		log.Errorf("quota history: not switching to %s: %v", path, err)
		return
	}
	h.path, h.samples, h.dirty, h.enabled = path, samples, false, true
	h.lastObserved = make(map[string]time.Time)
	for _, s := range samples {
		if s.At.After(h.lastObserved[s.AuthID]) {
			h.lastObserved[s.AuthID] = s.At
		}
	}
	log.Infof("quota history: recording to %s", path)
}

// Shutdown flushes pending data.
func (h *History) Shutdown() {
	if h == nil {
		return
	}
	if err := h.Flush(); err != nil {
		log.Errorf("quota history: shutdown flush failed: %v", err)
	}
}

// Run samples list() every sampleInterval until ctx is cancelled. Passes while
// the history is disabled are no-ops, so config reloads need no restart.
func (h *History) Run(ctx context.Context, list func() []*coreauth.Auth) {
	if h == nil || list == nil {
		return
	}
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for {
		h.Sample(list())
		if err := h.Flush(); err != nil {
			log.Errorf("quota history: flush failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sample records the windows of every credential whose snapshot is newer than
// the last one sampled for it, prunes old samples and returns how many were
// added.
func (h *History) Sample(auths []*coreauth.Auth) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enabled {
		return 0
	}
	added := 0
	for _, auth := range auths {
		if auth == nil || auth.ID == "" || !auth.Quota.ObservedAt.UTC().After(h.lastObserved[auth.ID]) {
			continue
		}
		h.lastObserved[auth.ID] = auth.Quota.ObservedAt.UTC()
		samples := parseSamples(auth)
		h.samples = append(h.samples, samples...)
		added += len(samples)
	}
	cutoff := h.now().Add(-retention)
	kept := h.samples[:0]
	for _, s := range h.samples {
		if !s.At.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	if len(kept) != len(h.samples) || added > 0 {
		h.dirty = true
	}
	h.samples = kept
	return added
}

// Query returns samples with from <= at < to (zero bound = open), oldest first.
func (h *History) Query(from, to time.Time) []Sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sortedSamples(from, to)
}

// sortedSamples copies samples within [from, to). Callers hold mu.
func (h *History) sortedSamples(from, to time.Time) []Sample {
	out := make([]Sample, 0, len(h.samples))
	for _, s := range h.samples {
		if (!from.IsZero() && s.At.Before(from)) || (!to.IsZero() && !s.At.Before(to)) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if a.AuthID != b.AuthID {
			return a.AuthID < b.AuthID
		}
		return a.WindowMinutes < b.WindowMinutes
	})
	return out
}

type fileFormat struct {
	Version int      `json:"version"`
	Samples []Sample `json:"samples"`
}

func load(path string) ([]Sample, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read quota history: %w", err)
	}
	var f fileFormat
	if err = json.Unmarshal(data, &f); err != nil || f.Version != fileVersion {
		// Keep the unreadable file for inspection instead of overwriting it.
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if errRename := os.Rename(path, backup); errRename != nil {
			return nil, fmt.Errorf("quota history %s unreadable (version %d, %v) and could not be moved aside: %w", path, f.Version, err, errRename)
		}
		log.Errorf("quota history: %s unreadable (version %d, %v); moved to %s and starting empty", path, f.Version, err, backup)
		return nil, nil
	}
	return f.Samples, nil
}

// Flush writes the history atomically (temp file + rename, 0600) when dirty.
func (h *History) Flush() error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	h.mu.Lock()
	if !h.dirty || h.path == "" {
		h.mu.Unlock()
		return nil
	}
	path := h.path
	snapshot := h.sortedSamples(time.Time{}, time.Time{})
	h.dirty = false
	h.mu.Unlock()

	if err := writeFile(path, fileFormat{Version: fileVersion, Samples: snapshot}); err != nil {
		h.mu.Lock()
		h.dirty = true
		h.mu.Unlock()
		return err
	}
	return nil
}

func writeFile(path string, f fileFormat) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode quota history: %w", err)
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create quota history dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp quota history: %w", err)
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
		return fmt.Errorf("write quota history: %w", errWrite)
	}
	return nil
}
