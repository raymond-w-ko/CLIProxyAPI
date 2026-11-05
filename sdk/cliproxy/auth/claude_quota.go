package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	log "github.com/sirupsen/logrus"
)

const claudeQuotaFreshness = time.Minute
const claudeQuotaRankAge = 5 * time.Minute

// ClaudeQuotaWindow is an observation, separate from confirmed request cooldowns.
// Unlimited is set only by an absent/null window in a valid full usage response.
type ClaudeQuotaWindow struct {
	Used      float64   `json:"used"`
	Reset     time.Time `json:"reset"`
	Observed  time.Time `json:"observed"`
	Unlimited bool      `json:"unlimited"`
}

// ClaudeQuotaSnapshot holds independently dated usage windows.
type ClaudeQuotaSnapshot map[string]ClaudeQuotaWindow

// ClaudeQuotaFetcher is an optional executor capability for standalone routing.
// It must not classify probe failures as inference failures.
type ClaudeQuotaFetcher interface {
	FetchClaudeQuota(context.Context, *Auth) (ClaudeQuotaSnapshot, error)
}

// ClaudeQuotaProbeError contains only safe status and retry information.
type ClaudeQuotaProbeError struct {
	Status  int
	RetryAt time.Time
}

func (e *ClaudeQuotaProbeError) Error() string {
	return fmt.Sprintf("Claude quota probe returned HTTP %d", e.Status)
}

// ParseClaudeQuotaUsage parses a complete OAuth usage response. Header utilization
// is a fraction; this endpoint's utilization is a percentage.
func ParseClaudeQuotaUsage(raw []byte, now time.Time) (ClaudeQuotaSnapshot, error) {
	if !ojson.ValidBytes(raw) || !ojson.ParseBytes(raw).IsObject() {
		return nil, fmt.Errorf("invalid Claude quota response")
	}
	doc := ojson.NewDoc(raw)
	weekly := doc.Get("seven_day")
	// An empty/error object must never imply unlimited quota.
	if !weekly.IsObject() {
		return nil, fmt.Errorf("Claude quota response lacks seven_day")
	}
	out := make(ClaudeQuotaSnapshot)
	for _, field := range []struct{ json, window string }{
		{"five_hour", "5h"}, {"seven_day", "7d"}, {"seven_day_opus", "opus"},
		{"seven_day_sonnet", "sonnet"}, {"seven_day_overage_included", "7d_oi"},
	} {
		value := doc.Get(field.json)
		if !value.Exists() || value.Type == ojson.Null {
			if field.window == "5h" {
				out["5h"] = ClaudeQuotaWindow{Unlimited: true, Observed: now}
			}
			continue
		}
		used := value.Get("utilization")
		resetValue := value.Get("resets_at")
		reset, errReset := time.Parse(time.RFC3339Nano, resetValue.String())
		// An unused window can exist without an active reset. It is not unlimited.
		if resetValue.Raw == "null" && used.Type == ojson.Number && used.Float() == 0 {
			errReset = nil
		}
		if !value.IsObject() || used.Type != ojson.Number || !validClaudeUtilization(used.Float()/100) || errReset != nil {
			return nil, fmt.Errorf("invalid Claude quota window %s", field.json)
		}
		out[field.window] = ClaudeQuotaWindow{Used: min(used.Float()/100, 1), Reset: reset, Observed: now}
	}
	// Newer responses can carry scoped windows. Only explicit model names are mapped.
	for _, value := range doc.Get("limits").Array() {
		name := strings.ToLower(value.Get("scope.model.display_name").String())
		if value.Get("group").String() != "weekly" || value.Get("kind").String() != "weekly_scoped" || !strings.Contains(name, "fable") {
			continue
		}
		used := value.Get("percent")
		reset, errReset := time.Parse(time.RFC3339Nano, value.Get("resets_at").String())
		if used.Type == ojson.Number && validClaudeUtilization(used.Float()/100) && errReset == nil {
			out["7d_oi"] = ClaudeQuotaWindow{Used: min(used.Float()/100, 1), Reset: reset, Observed: now}
		}
	}
	return out, nil
}

func validClaudeUtilization(used float64) bool {
	return !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0
}

func claudeQuotaHeaders(headers http.Header, now time.Time) ClaudeQuotaSnapshot {
	out := make(ClaudeQuotaSnapshot)
	for _, name := range []string{"5h", "7d", "7d_oi"} {
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		used, errUsed := strconv.ParseFloat(headers.Get(prefix+"Utilization"), 64)
		reset, errReset := strconv.ParseInt(headers.Get(prefix+"Reset"), 10, 64)
		if errUsed != nil || errReset != nil || !validClaudeUtilization(used) || reset <= 0 {
			continue
		}
		out[name] = ClaudeQuotaWindow{Used: min(used, 1), Reset: time.Unix(reset, 0), Observed: now}
	}
	return out
}

type claudeQuotaRecord struct {
	Windows      ClaudeQuotaSnapshot `json:"windows"`
	Next         time.Time           `json:"next"`
	AuthFailures int                 `json:"auth_failures"`
	Failures     int                 `json:"failures"`
	Paused       bool                `json:"paused"`
	loaded       bool
	version      uint64
	dirty        bool
	saved        time.Time
}

func (r *claudeQuotaRecord) markDirty() { r.dirty = true; r.version++ }

type claudeQuotaCache struct {
	mu      sync.Mutex
	records map[string]*claudeQuotaRecord
}

func newClaudeQuotaCache() *claudeQuotaCache {
	return &claudeQuotaCache{records: make(map[string]*claudeQuotaRecord)}
}

// Including the grant prevents stale in-flight probes and reused auth IDs from
// exposing another account's quota. Token replacement also clears the failure gate.
func claudeQuotaKey(a *Auth) string {
	return "claude-quota:v1:" + kvstore.HashKeyPart(a.ID+"\x00"+AuthGrantFingerprint(a))
}
func isClaudeQuotaAuth(a *Auth) bool {
	return a != nil && strings.EqualFold(a.Provider, "claude") && !isAPIKeyAuth(a) && authAccessToken(a) != ""
}
func (c *claudeQuotaCache) recordLocked(key string) *claudeQuotaRecord {
	r := c.records[key]
	if r == nil {
		r = &claudeQuotaRecord{Windows: make(ClaudeQuotaSnapshot)}
		c.records[key] = r
	}
	return r
}
func mergeClaudeQuota(dst, src ClaudeQuotaSnapshot) {
	for name, w := range src {
		if old, ok := dst[name]; !ok || !w.Observed.Before(old.Observed) {
			dst[name] = w
		}
	}
}
func (c *claudeQuotaCache) observe(a *Auth, headers http.Header, now time.Time) {
	if c == nil || !isClaudeQuotaAuth(a) {
		return
	}
	windows := claudeQuotaHeaders(headers, now)
	if len(windows) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.recordLocked(claudeQuotaKey(a))
	mergeClaudeQuota(r.Windows, windows)
	r.markDirty()
}
func (c *claudeQuotaCache) snapshot(a *Auth) ClaudeQuotaSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(ClaudeQuotaSnapshot)
	if r := c.records[claudeQuotaKey(a)]; r != nil {
		mergeClaudeQuota(out, r.Windows)
	}
	return out
}

func claudeQuotaWindowFresh(w ClaudeQuotaWindow, now time.Time, age time.Duration) bool {
	return !w.Observed.IsZero() && !w.Observed.After(now) && now.Sub(w.Observed) < age && (w.Unlimited || w.Reset.After(now) || (w.Reset.IsZero() && w.Used == 0))
}
func claudeQuotaProbeDue(r *claudeQuotaRecord, now time.Time) bool {
	if r.Paused || r.Next.After(now) {
		return false
	}
	for _, name := range []string{"5h", "7d"} {
		if !claudeQuotaWindowFresh(r.Windows[name], now, claudeQuotaFreshness) {
			return true
		}
	}
	return false
}

// load and save run only on the single quota worker. Passive observations never
// perform disk I/O or acquire the manager lock through this cache.
func (c *claudeQuotaCache) load(ctx context.Context, a *Auth) {
	key := claudeQuotaKey(a)
	c.mu.Lock()
	r := c.recordLocked(key)
	loaded := r.loaded
	r.loaded = true
	c.mu.Unlock()
	if loaded {
		return
	}
	client, enabled, errClient := kvstore.CurrentKVClient()
	if !enabled || errClient != nil {
		return
	}
	raw, found, errGet := client.KVGet(ctx, key)
	if errGet != nil || !found || !ojson.ValidBytes(raw) {
		return
	}
	doc := ojson.NewDoc(raw)
	windows := make(ClaudeQuotaSnapshot)
	doc.Get("windows").ForEach(func(k, v ojson.Result) bool {
		observed, errObserved := time.Parse(time.RFC3339Nano, v.Get("observed").String())
		reset, _ := time.Parse(time.RFC3339Nano, v.Get("reset").String())
		used := v.Get("used").Float()
		unlimited := v.Get("unlimited").Bool()
		name := k.String()
		known := name == "5h" || name == "7d" || name == "7d_oi" || name == "opus" || name == "sonnet"
		if known && (!unlimited || name == "5h") && v.Get("used").Type == ojson.Number && errObserved == nil && validClaudeUtilization(used) && (unlimited || !reset.IsZero() || used == 0) {
			windows[strings.Clone(k.String())] = ClaudeQuotaWindow{Used: min(used, 1), Reset: reset, Observed: observed, Unlimited: unlimited}
		}
		return true
	})
	next, _ := time.Parse(time.RFC3339Nano, doc.Get("next").String())
	c.mu.Lock()
	defer c.mu.Unlock()
	mergeClaudeQuota(r.Windows, windows)
	r.Next = next
	r.AuthFailures = int(doc.Get("auth_failures").Int())
	r.Failures = int(doc.Get("failures").Int())
	r.AuthFailures = max(0, min(r.AuthFailures, 3))
	r.Failures = max(0, min(r.Failures, 100))
	r.Paused = doc.Get("paused").Bool()
}
func (c *claudeQuotaCache) save(ctx context.Context, a *Auth, now time.Time, force bool) {
	c.saveWith(ctx, a, now, force, saveClaudeQuotaKV)
}

func saveClaudeQuotaKV(ctx context.Context, key string, raw []byte) error {
	client, enabled, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		return errClient
	}
	if !enabled {
		return nil
	}
	_, errSet := client.KVSet(ctx, key, raw, kvstore.KVSetOptions{EX: 8 * 24 * time.Hour})
	return errSet
}

// saveWith is called by the single worker, or shutdown after the worker joins.
// Only serialization holds the cache lock. Disk commits cannot block requests.
func (c *claudeQuotaCache) saveWith(ctx context.Context, a *Auth, now time.Time, force bool, write func(context.Context, string, []byte) error) {
	key := claudeQuotaKey(a)
	c.mu.Lock()
	r := c.recordLocked(key)
	if !r.dirty || (!force && now.Sub(r.saved) < time.Minute) {
		c.mu.Unlock()
		return
	}
	version := r.version
	doc := ojson.NewDocString("{}")
	if errSet := doc.Set("windows", r.Windows); errSet != nil {
		c.mu.Unlock()
		return
	}
	_ = doc.Set("next", r.Next)
	_ = doc.Set("auth_failures", r.AuthFailures)
	_ = doc.Set("failures", r.Failures)
	_ = doc.Set("paused", r.Paused)
	raw := doc.Bytes()
	c.mu.Unlock()
	if errWrite := write(ctx, key, raw); errWrite != nil {
		log.WithError(errWrite).Warn("could not save Claude routing quota")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.records[key] != r {
		return
	}
	// Throttle writes even when a newer observation arrived during this commit.
	r.saved = now
	if r.version == version {
		r.dirty = false
	}
}

// A full response supersedes older omitted windows, but never a newer passive
// observation. Expired windows carry no current routing information.
func applyClaudeQuotaFull(dst, src ClaudeQuotaSnapshot, started time.Time) {
	for name, w := range dst {
		_, present := src[name]
		if !w.Observed.After(started) && (!present || (!w.Reset.IsZero() && !w.Reset.After(started))) {
			delete(dst, name)
		}
	}
	next := make(ClaudeQuotaSnapshot, len(src))
	for name, w := range src {
		if !w.Reset.IsZero() && !w.Reset.After(started) {
			continue
		}
		w.Observed = started
		next[name] = w
	}
	mergeClaudeQuota(dst, next)
}

// Only the manager's successful refresh path carries observations forward. A
// manual account replacement still starts with unknown quota and an open gate.
func (m *Manager) carryClaudeQuotaRefresh(base, updated *Auth) {
	if m.claudeQuota == nil || !isClaudeQuotaAuth(base) || !isClaudeQuotaAuth(updated) || base.ID != updated.ID || base.RegistrationEpoch != updated.RegistrationEpoch {
		return
	}
	oldKey, newKey := claudeQuotaKey(base), claudeQuotaKey(updated)
	if oldKey == newKey {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[updated.ID]
	if current == nil || current.RegistrationEpoch != updated.RegistrationEpoch || claudeQuotaKey(current) != newKey {
		return
	}
	m.claudeQuota.mu.Lock()
	defer m.claudeQuota.mu.Unlock()
	old := m.claudeQuota.records[oldKey]
	if old == nil {
		return
	}
	next := m.claudeQuota.recordLocked(newKey)
	mergeClaudeQuota(next.Windows, old.Windows)
	if old.Next.After(next.Next) {
		next.Next = old.Next
	}
	next.loaded = true
	next.markDirty()
}
