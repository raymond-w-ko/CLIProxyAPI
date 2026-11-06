package auth

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaTestAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "sk-ant-oat01-" + id, "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}}
}
func quotaTestWindows(now time.Time, until time.Duration) ClaudeQuotaSnapshot {
	return ClaudeQuotaSnapshot{"5h": {Unlimited: true, Observed: now}, "7d": {Used: .5, Reset: now.Add(until), Observed: now}}
}
func quotaTestSet(m *Manager, a *Auth, windows ClaudeQuotaSnapshot) {
	m.claudeQuota.mu.Lock()
	defer m.claudeQuota.mu.Unlock()
	r := m.claudeQuota.recordLocked(claudeQuotaKey(a))
	mergeClaudeQuota(r.Windows, windows)
	r.markDirty()
}
func TestClaudeQuotaParse(t *testing.T) {
	now := time.Now()
	for _, five := range []string{`"five_hour":null,`, ``} {
		windows, errParse := ParseClaudeQuotaUsage([]byte(`{`+five+`"seven_day":{"utilization":88,"resets_at":"2030-01-02T03:04:05Z"},"limits":[{"group":"weekly","kind":"weekly_scoped","percent":100,"resets_at":"2030-01-03T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`), now)
		if errParse != nil || !windows["5h"].Unlimited || windows["7d"].Used != .88 || windows["7d_oi"].Used != 1 {
			t.Fatalf("parse = %+v, %v", windows, errParse)
		}
	}
	for _, raw := range []string{`{}`, `{"error":"denied"}`, `{"seven_day":null}`, `{"seven_day":{"utilization":-1,"resets_at":"2030-01-01T00:00:00Z"}}`, `{"seven_day":{"utilization":"4","resets_at":"2030-01-01T00:00:00Z"}}`, `{"seven_day":{"utilization":4,"resets_at":"invalid"}}`} {
		if _, errParse := ParseClaudeQuotaUsage([]byte(raw), now); errParse == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	headers := http.Header{"Anthropic-Ratelimit-Unified-7d-Status": {"allowed"}}
	if got := claudeQuotaHeaders(headers, now); len(got) != 0 {
		t.Fatalf("sparse header fabricated windows: %v", got)
	}
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "NaN")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", "2000000000")
	if got := claudeQuotaHeaders(headers, now); len(got) != 0 {
		t.Fatal("accepted nonfinite utilization")
	}
}

func TestSoonestResetFirstSelection(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := &SoonestResetFirstSelector{}
	m := NewManager(nil, s, nil)
	a, b := quotaTestAuth("a"), quotaTestAuth("b")
	quotaTestSet(m, a, quotaTestWindows(now, 4*24*time.Hour))
	quotaTestSet(m, b, quotaTestWindows(now, 16*time.Hour))
	opts := cliproxyexecutor.Options{}
	pick := func(want string) {
		t.Helper()
		got, errPick := s.Pick(ctx, "claude", "claude-fable", opts, []*Auth{a, b})
		if errPick != nil || got.ID != want {
			t.Fatalf("pick = %v, %v; want %s", got, errPick, want)
		}
	}
	pick("b")
	// Remaining percentage does not determine the order.
	quotaTestSet(m, b, ClaudeQuotaSnapshot{"7d": {Used: .99, Reset: now.Add(16 * time.Hour), Observed: now}})
	pick("b")
	quotaTestSet(m, b, ClaudeQuotaSnapshot{"5h": {Used: 1, Reset: now.Add(time.Hour), Observed: now}})
	pick("a")
	opts.Metadata = map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "b"}
	got, errPick := s.Pick(ctx, "claude", "claude-fable", opts, []*Auth{b})
	if errPick != nil || got.ID != "b" {
		t.Fatal("probe changed pinned owner", errPick)
	}
	opts.Metadata = nil
	quotaTestSet(m, b, ClaudeQuotaSnapshot{"5h": {Unlimited: true, Observed: now}, "7d_oi": {Used: 1, Reset: now.Add(time.Hour), Observed: now}})
	pick("a")
	a.Attributes = map[string]string{"priority": "10"}
	b.Attributes = map[string]string{"priority": "1"}
	pick("a")
	delete(a.Attributes, "priority")
	delete(b.Attributes, "priority")
	quotaTestSet(m, a, ClaudeQuotaSnapshot{"7d": {Used: 1, Reset: now.Add(time.Hour), Observed: now}})
	if _, errPick := s.Pick(ctx, "claude", "claude-fable", opts, []*Auth{a, b}); errPick == nil {
		t.Fatal("selected exhausted account")
	}
	// Unknown data falls back to round robin. Expired data does not invent a reset.
	unknown := quotaTestAuth("unknown")
	if got, errPick := s.Pick(ctx, "claude", "claude-fable", opts, []*Auth{a, b, unknown}); errPick != nil || got.ID != "unknown" {
		t.Fatalf("unknown fallback = %v, %v", got, errPick)
	}
	for _, w := range []ClaudeQuotaWindow{{Used: 1, Observed: now.Add(-25 * time.Hour), Reset: now.Add(time.Hour)}, {Used: 1, Observed: now, Reset: now.Add(-time.Hour)}} {
		if _, known, blocked := claudeQuotaRank(ClaudeQuotaSnapshot{"7d": w}, "claude-fable", now); known || blocked {
			t.Fatal("stale data affected rank")
		}
	}
}

type quotaProbeExecutor struct {
	schedulerTestExecutor
	calls        int
	fetch        func(context.Context, *Auth) (ClaudeQuotaSnapshot, error)
	refreshCalls int
}

func (e *quotaProbeExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	e.refreshCalls++
	a = a.Clone()
	a.Metadata["access_token"] = authAccessToken(a) + "-refreshed"
	a.Metadata["expired"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	return a, nil
}
func (e *quotaProbeExecutor) FetchClaudeQuota(ctx context.Context, a *Auth) (ClaudeQuotaSnapshot, error) {
	e.calls++
	return e.fetch(ctx, a)
}
func quotaProbeManager(t *testing.T, fetch func(context.Context, *Auth) (ClaudeQuotaSnapshot, error)) (*Manager, *Auth, *quotaProbeExecutor) {
	t.Helper()
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	a := quotaTestAuth(t.Name())
	registered, errRegister := m.Register(WithSkipPersist(context.Background()), a)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	e := &quotaProbeExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}, fetch: fetch}
	m.RegisterExecutor(e)
	return m, registered, e
}
func TestClaudeQuotaPollingFreshnessAndBackoff(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return quotaTestWindows(now, 24*time.Hour), nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	m.refreshOneClaudeQuota(ctx, now.Add(15*time.Minute-time.Second))
	if e.calls != 1 {
		t.Fatalf("calls=%d", e.calls)
	}
	h := http.Header{}
	for _, name := range []string{"5h", "7d"} {
		h.Set("Anthropic-Ratelimit-Unified-"+name+"-Utilization", "0.5")
		h.Set("Anthropic-Ratelimit-Unified-"+name+"-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))
	}
	m.claudeQuota.observe(a, h, now.Add(16*time.Minute))
	m.refreshOneClaudeQuota(ctx, now.Add(16*time.Minute))
	if e.calls != 1 {
		t.Fatal("fresh headers did not suppress probe")
	}
	m.claudeQuota.observe(a, http.Header{"Anthropic-Ratelimit-Unified-7d-Status": {"allowed"}}, now.Add(21*time.Minute))
	m.refreshOneClaudeQuota(ctx, now.Add(21*time.Minute))
	if e.calls != 2 {
		t.Fatalf("stale data not refreshed: %d", e.calls)
	}
	e.fetch = func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 429, RetryAt: now.Add(90 * time.Minute)}
	}
	m.refreshOneClaudeQuota(ctx, now.Add(36*time.Minute))
	m.refreshOneClaudeQuota(ctx, now.Add(89*time.Minute))
	if e.calls != 3 {
		t.Fatalf("ignored Retry-After: %d", e.calls)
	}
	if got := m.claudeQuota.snapshot(a)["7d"].Used; got != .5 {
		t.Fatal("failure replaced successful data", got)
	}
}

func TestClaudeQuotaAuthFailureGate(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 403}
	})
	for _, minutes := range []int{0, 15, 45, 120, 200} {
		m.refreshOneClaudeQuota(ctx, now.Add(time.Duration(minutes)*time.Minute))
	}
	if e.calls != 3 {
		t.Fatalf("auth calls = %d", e.calls)
	}
	current, _ := m.GetByID(a.ID)
	if current.Disabled || current.Unavailable || current.Quota.Exceeded {
		t.Fatal("probe failure changed inference state")
	}
	m.mu.Lock()
	m.auths[a.ID].Metadata["access_token"] = "sk-ant-oat01-replaced"
	m.mu.Unlock()
	m.refreshOneClaudeQuota(ctx, now.Add(201*time.Minute))
	if e.calls != 4 {
		t.Fatal("credential replacement did not clear pause")
	}
}
func TestClaudeQuotaRejectsStaleProbe(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var m *Manager
	m, a, _ := quotaProbeManager(t, func(_ context.Context, used *Auth) (ClaudeQuotaSnapshot, error) {
		m.mu.Lock()
		m.auths[used.ID].Metadata["access_token"] = "sk-ant-oat01-other"
		m.mu.Unlock()
		return quotaTestWindows(now, time.Hour), nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	current, _ := m.GetByID(a.ID)
	if len(m.claudeQuota.snapshot(current)) != 0 || len(m.claudeQuota.snapshot(a)) != 0 {
		t.Fatal("accepted stale probe")
	}
}
func TestClaudeQuotaPassiveWinsInFlight(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var m *Manager
	m, a, _ := quotaProbeManager(t, func(_ context.Context, a *Auth) (ClaudeQuotaSnapshot, error) {
		quotaTestSet(m, a, ClaudeQuotaSnapshot{"7d": {Used: .9, Reset: now.Add(time.Hour), Observed: now.Add(time.Second)}})
		return quotaTestWindows(now, 24*time.Hour), nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	if got := m.claudeQuota.snapshot(a)["7d"].Used; got != .9 {
		t.Fatal("late probe overwrote newer headers", got)
	}
}
func TestClaudeQuotaPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	if errConfig := kvstore.ConfigureLocal(true, path); errConfig != nil {
		t.Fatal(errConfig)
	}
	t.Cleanup(func() {
		if errClose := kvstore.ShutdownLocal(); errClose != nil {
			t.Error(errClose)
		}
	})
	now := time.Now()
	ctx := context.Background()
	a := quotaTestAuth("persistent")
	cache := newClaudeQuotaCache()
	cache.mu.Lock()
	r := cache.recordLocked(claudeQuotaKey(a))
	r.Windows = quotaTestWindows(now, time.Hour)
	r.AuthFailures = 3
	r.Paused = true
	r.markDirty()
	cache.mu.Unlock()
	cache.save(ctx, a, now, true)
	if errClose := kvstore.ShutdownLocal(); errClose != nil {
		t.Fatal(errClose)
	}
	if errConfig := kvstore.ConfigureLocal(true, path); errConfig != nil {
		t.Fatal(errConfig)
	}
	restored := newClaudeQuotaCache()
	restored.load(ctx, a)
	restored.mu.Lock()
	defer restored.mu.Unlock()
	record := restored.recordLocked(claudeQuotaKey(a))
	if !record.Paused || record.AuthFailures != 3 || record.Windows["7d"].Used != .5 || claudeQuotaProbeDue(record, now) {
		t.Fatalf("restored state = %+v", record)
	}
}
func TestSoonestResetDurableOwner(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &SoonestResetFirstSelector{}}))
	t.Cleanup(func() { m.Selector().(StoppableSelector).Stop() })
	now := time.Now()
	for i, id := range ids {
		m.mu.Lock()
		m.auths[id].Metadata = quotaTestAuth(id).Metadata
		a := m.auths[id].Clone()
		m.mu.Unlock()
		quotaTestSet(m, a, quotaTestWindows(now, time.Duration(2-i)*24*time.Hour))
	}
	owner, errPick := durableTestPick(m, "model-one", durableTestOptions())
	if errPick != nil || owner.ID != ids[1] {
		t.Fatalf("initial owner = %v %v", owner, errPick)
	}
	// Ranking flips and polling reports exhaustion. Neither can revoke ownership.
	quotaTestSet(m, owner, ClaudeQuotaSnapshot{"7d": {Used: 1, Reset: now.Add(5 * 24 * time.Hour), Observed: now}})
	owner, errPick = durableTestPick(m, "model-two", durableTestOptions())
	if errPick != nil || owner.ID != ids[1] {
		t.Fatal("changed durable owner", errPick)
	}
	m.MarkResult(context.Background(), Result{AuthID: ids[1], Provider: "claude", Model: "model-two", Error: &Error{Code: "credential_quota", HTTPStatus: 429, Message: "quota"}, CredentialScope: true, RetryAfter: durationPtr(time.Hour)})
	owner, errPick = durableTestPick(m, "model-two", durableTestOptions())
	if errPick != nil || owner.ID != ids[0] {
		t.Fatalf("quota migration = %v %v", owner, errPick)
	}
	restarted, _ := durableTestManager(t, path)
	owner, errPick = durableTestPick(restarted, "model-one", durableTestOptions())
	if errPick != nil || owner.ID != ids[0] {
		t.Fatal("restart lost replacement owner", errPick)
	}
}
func TestClaudeQuotaWorkerShutdown(t *testing.T) {
	entered := make(chan struct{})
	m, _, _ := quotaProbeManager(t, func(ctx context.Context, _ *Auth) (ClaudeQuotaSnapshot, error) {
		close(entered)
		<-ctx.Done()
		return nil, errors.New("canceled")
	})
	m.StartClaudeQuotaRefresh(context.Background())
	<-entered
	m.StopClaudeQuotaRefresh()
	// A completed worker can start again without leaking a goroutine.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	m.StartClaudeQuotaRefresh(canceled)
	m.StopClaudeQuotaRefresh()
}

func durationPtr(value time.Duration) *time.Duration { return &value }

func TestClaudeQuotaTerminalGrantSkipsProbe(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		t.Fatal("probed terminal grant")
		return nil, nil
	})
	m.mu.Lock()
	m.auths[a.ID].Metadata["refresh_token"] = "rejected"
	m.auths[a.ID].RejectedRefreshToken = "rejected"
	m.auths[a.ID].RequiresSignIn = true
	m.mu.Unlock()
	m.refreshOneClaudeQuota(ctx, now)
	if e.calls != 0 {
		t.Fatal(e.calls)
	}
}
func TestSoonestResetAliasAndPriorities(t *testing.T) {
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	s := &SoonestResetFirstSelector{}
	m.SetSelector(s)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "claude-fable-5", Alias: "model-one"}}})
	now := time.Now()
	for i, id := range ids {
		m.mu.Lock()
		m.auths[id].Metadata = quotaTestAuth(id).Metadata
		a := m.auths[id].Clone()
		m.mu.Unlock()
		quotaTestSet(m, a, quotaTestWindows(now, time.Duration(i+1)*time.Hour))
		if i == 0 {
			quotaTestSet(m, a, ClaudeQuotaSnapshot{"7d_oi": {Used: 1, Reset: now.Add(time.Hour), Observed: now}})
		}
	}
	got, errPick := durableTestPick(m, "model-one", durableTestOptions())
	if errPick != nil || got.ID != ids[1] {
		t.Fatalf("aliased scoped quota = %v, %v", got, errPick)
	}
	// An exhausted top-priority account must not hide a healthy lower tier.
	m.mu.Lock()
	m.auths[ids[0]].Attributes = map[string]string{"priority": "10"}
	m.mu.Unlock()
	got, errPick = durableTestPick(m, "model-one", durableTestOptions())
	if errPick != nil || got.ID != ids[1] {
		t.Fatalf("exhausted high priority = %v, %v", got, errPick)
	}
}
func TestClaudeQuotaMarkResultPassiveObservation(t *testing.T) {
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		t.Fatal("fresh observation triggered probe")
		return nil, nil
	})
	now := time.Now()
	headers := http.Header{}
	for _, name := range []string{"5h", "7d"} {
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Utilization", "0.25")
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))
	}
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, headers)
	m.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Success: true})
	m.refreshOneClaudeQuota(ctx, time.Now())
	if e.calls != 0 || m.claudeQuota.snapshot(a)["7d"].Used != .25 {
		t.Fatal("normal headers did not reach routing cache")
	}
}

func TestClaudeQuotaValidToken401DoesNotRefreshInference(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 401}
	})
	for _, minutes := range []int{0, 15, 45, 180} {
		m.refreshOneClaudeQuota(ctx, now.Add(time.Duration(minutes)*time.Minute))
	}
	if e.calls != 3 || e.refreshCalls != 0 {
		t.Fatalf("probes=%d refreshes=%d", e.calls, e.refreshCalls)
	}
	current, _ := m.GetByID(a.ID)
	if current.Disabled || current.Unavailable || current.RequiresSignIn || current.RejectedAccessToken != "" {
		t.Fatal("quota rejection invalidated inference")
	}
}
func TestClaudeQuotaExpired401UsesCoordinatedRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 401}
	})
	m.mu.Lock()
	m.auths[a.ID].Metadata["expired"] = now.Add(-time.Hour).Format(time.RFC3339)
	m.auths[a.ID].Metadata["refresh_token"] = "refresh"
	m.mu.Unlock()
	for _, minutes := range []int{0, 15, 30, 60, 180} {
		m.refreshOneClaudeQuota(ctx, now.Add(time.Duration(minutes)*time.Minute))
	}
	if e.calls != 4 || e.refreshCalls != 1 {
		t.Fatalf("probes=%d refreshes=%d", e.calls, e.refreshCalls)
	}
	current, _ := m.GetByID(a.ID)
	if current.RejectedAccessToken != "" || current.Quota.Exceeded {
		t.Fatal("probe changed inference availability")
	}
}

func TestClaudeQuotaOverLimitIsExhausted(t *testing.T) {
	now := time.Now()
	full, errParse := ParseClaudeQuotaUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":101,"resets_at":"2030-01-01T00:00:00Z"},"limits":[{"group":"weekly","kind":"weekly_scoped","percent":102,"resets_at":"2030-01-01T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`), now)
	if errParse != nil || full["7d"].Used != 1 || full["7d_oi"].Used != 1 {
		t.Fatalf("over-limit full response = %v, %v", full, errParse)
	}
	if _, _, blocked := claudeQuotaRank(full, "claude-fable", now); !blocked {
		t.Fatal("over-limit quota did not block new selection")
	}
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	a := quotaTestAuth(t.Name())
	quotaTestSet(m, a, quotaTestWindows(now, 24*time.Hour))
	quotaTestSet(m, a, ClaudeQuotaSnapshot{"7d_oi": {Used: .4, Reset: now.Add(time.Hour), Observed: now.Add(-time.Second)}})
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", "1.02")
	headers.Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	m.claudeQuota.observe(a, headers, now)
	if got := m.claudeQuota.snapshot(a)["7d_oi"].Used; got != 1 {
		t.Fatal("over-limit header did not replace healthy observation", got)
	}
	if _, _, blocked := claudeQuotaRank(m.claudeQuota.snapshot(a), "claude-fable", now); !blocked {
		t.Fatal("over-limit Fable header did not block selection")
	}
}

func TestClaudeQuotaUnusedWindowWithNullReset(t *testing.T) {
	now := time.Now()
	for _, weekly := range []string{`{"utilization":20,"resets_at":"2030-01-01T00:00:00Z"}`, `{"utilization":0,"resets_at":null}`} {
		windows, errParse := ParseClaudeQuotaUsage([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":`+weekly+`}`), now)
		if errParse != nil || windows["5h"].Unlimited || !windows["5h"].Reset.IsZero() || !claudeQuotaWindowFresh(windows["5h"], now, time.Minute) {
			t.Fatalf("idle window = %v, %v", windows, errParse)
		}
		_, known, blocked := claudeQuotaRank(windows, "claude-sonnet", now)
		if blocked || known != windows["7d"].Reset.After(now) {
			t.Fatal("idle window invented a weekly reset")
		}
	}
}

func TestClaudeQuotaSideWindowsDoNotForcePolling(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		windows := quotaTestWindows(now, 24*time.Hour)
		windows["opus"] = ClaudeQuotaWindow{Used: .1, Reset: now.Add(24 * time.Hour), Observed: now}
		windows["sonnet"] = ClaudeQuotaWindow{Used: .2, Reset: now.Add(24 * time.Hour), Observed: now}
		return windows, nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	headers := http.Header{}
	for _, name := range []string{"5h", "7d"} {
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Utilization", "0.3")
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))
	}
	for minute := 1; minute <= 120; minute++ {
		at := now.Add(time.Duration(minute) * time.Minute)
		m.claudeQuota.observe(a, headers, at)
		m.refreshOneClaudeQuota(ctx, at)
	}
	if e.calls != 1 {
		t.Fatalf("side windows forced %d reads", e.calls)
	}
	dst := ClaudeQuotaSnapshot{
		"opus":   {Used: .1, Reset: now.Add(time.Hour), Observed: now.Add(-time.Minute)},
		"sonnet": {Used: .2, Reset: now.Add(-time.Second), Observed: now.Add(-time.Minute)},
		"7d_oi":  {Used: .7, Reset: now.Add(time.Hour), Observed: now.Add(time.Second)},
	}
	applyClaudeQuotaFull(dst, quotaTestWindows(now, time.Hour), now)
	if _, ok := dst["opus"]; ok {
		t.Fatal("full response kept omitted old window")
	}
	if _, ok := dst["sonnet"]; ok {
		t.Fatal("full response kept expired window")
	}
	if dst["7d_oi"].Used != .7 {
		t.Fatal("full response pruned a newer passive observation")
	}
}

func TestClaudeQuotaSaveDoesNotBlockRequestCompletion(t *testing.T) {
	m, a, _ := quotaProbeManager(t, nil)
	now := time.Now()
	quotaTestSet(m, a, quotaTestWindows(now, 24*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var persisted []byte
	go func() {
		defer close(finished)
		m.claudeQuota.saveWith(ctx, a, now, true, func(ctx context.Context, _ string, raw []byte) error {
			persisted = append([]byte(nil), raw...)
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	headers := http.Header{}
	for _, name := range []string{"5h", "7d"} {
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Utilization", "0.25")
		headers.Set("Anthropic-Ratelimit-Unified-"+name+"-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))
	}
	requestCtx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(requestCtx, headers)
	completed := make(chan struct{})
	go func() {
		m.MarkResult(requestCtx, Result{AuthID: a.ID, Provider: "claude", Success: true})
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("disk write blocked request completion")
	}
	if m.claudeQuota.snapshot(a)["7d"].Used != .25 {
		t.Fatal("disk write blocked selector snapshot")
	}
	close(release)
	<-finished
	m.claudeQuota.mu.Lock()
	dirty := m.claudeQuota.recordLocked(claudeQuotaKey(a)).dirty
	m.claudeQuota.mu.Unlock()
	if !dirty {
		t.Fatal("write cleared a newer observation's dirty flag")
	}
	if got := ojson.GetBytes(persisted, "windows.7d.used").Float(); got != .5 {
		t.Fatal("initial write did not use its captured snapshot", got)
	}
	m.claudeQuota.saveWith(ctx, a, now.Add(time.Minute), false, func(_ context.Context, _ string, raw []byte) error {
		persisted = append([]byte(nil), raw...)
		return nil
	})
	if got := ojson.GetBytes(persisted, "windows.7d.used").Float(); got != .25 {
		t.Fatal("newer observation was not saved", got)
	}
}

func TestClaudeQuotaSuccessfulRefreshKeepsObservations(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	now := time.Now()
	m, a, _ := quotaProbeManager(t, nil)
	quotaTestSet(m, a, quotaTestWindows(now, time.Hour))
	m.claudeQuota.mu.Lock()
	old := m.claudeQuota.recordLocked(claudeQuotaKey(a))
	old.Paused = true
	old.AuthFailures = 3
	old.Next = now.Add(time.Minute)
	old.markDirty()
	m.claudeQuota.mu.Unlock()
	updated := a.Clone()
	updated.Metadata["access_token"] = "sk-ant-oat01-refreshed"
	saved, errUpdate := m.UpdateRefreshedAuth(ctx, a, updated)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if got := m.claudeQuota.snapshot(saved)["7d"].Used; got != .5 {
		t.Fatal("normal refresh lost observations", got)
	}
	m.claudeQuota.mu.Lock()
	next := m.claudeQuota.recordLocked(claudeQuotaKey(saved))
	paused, failures := next.Paused, next.AuthFailures
	m.claudeQuota.mu.Unlock()
	if paused || failures != 0 {
		t.Fatal("successful refresh carried old auth failures")
	}
	replacement := saved.Clone()
	replacement.Metadata["access_token"] = "sk-ant-oat01-manual-replacement"
	replaced, errUpdate := m.Update(ctx, replacement)
	if errUpdate != nil || len(m.claudeQuota.snapshot(replaced)) != 0 {
		t.Fatal("manual replacement reused another grant's observations", errUpdate)
	}
}
