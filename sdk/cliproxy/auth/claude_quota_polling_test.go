package auth

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func TestClaudeQuotaTrustedAccountUsesWeeklyHeaders(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	m, a, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return quotaTestWindows(now, 7*24*time.Hour), nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	for _, offset := range []time.Duration{time.Minute, 16 * time.Minute, time.Hour, 12 * time.Hour, 23 * time.Hour} {
		at := now.Add(offset)
		headers := http.Header{}
		headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.4")
		headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10))
		m.claudeQuota.observe(a, headers, at)
		m.refreshOneClaudeQuota(ctx, at)
	}
	if e.calls != 1 {
		t.Fatalf("trusted account forced %d quota reads", e.calls)
	}
	m.refreshOneClaudeQuota(ctx, now.Add(24*time.Hour))
	if e.calls != 2 {
		t.Fatal("five-hour capability never revalidated")
	}
}

func TestClaudeQuotaIdleAndIntermittentFailuresStaySlow(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	for _, intermittent := range []bool{false, true} {
		t.Run(fmt.Sprint(intermittent), func(t *testing.T) {
			var attempts []time.Time
			var at time.Time
			m, _, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
				attempts = append(attempts, at)
				if intermittent && len(attempts)%2 == 0 {
					return nil, &ClaudeQuotaProbeError{Status: 500}
				}
				return quotaTestWindows(now, 24*time.Hour), nil
			})
			for minute := 0; minute < 60; minute++ {
				at = now.Add(time.Duration(minute) * time.Minute)
				m.refreshOneClaudeQuota(ctx, at)
			}
			if e.calls != 4 {
				t.Fatalf("reads per hour=%d, want 4", e.calls)
			}
			for i := 1; i < len(attempts); i++ {
				if attempts[i].Sub(attempts[i-1]) < 15*time.Minute {
					t.Fatal("success reset polling to one minute")
				}
			}
		})
	}
}

func TestClaudeQuotaPool429SurvivesRestartAndCredentialChange(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "kv.db")
	if errConfig := kvstore.ConfigureLocal(true, path); errConfig != nil {
		t.Fatal(errConfig)
	}
	t.Cleanup(func() {
		if errClose := kvstore.ShutdownLocal(); errClose != nil {
			t.Error(errClose)
		}
	})
	m, _, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 429}
	})
	second := quotaTestAuth(t.Name() + "-second")
	if _, errRegister := m.Register(WithSkipPersist(ctx), second); errRegister != nil {
		t.Fatal(errRegister)
	}
	m.refreshOneClaudeQuota(ctx, now)
	for _, offset := range []time.Duration{time.Second, time.Minute, 15 * time.Minute, 29 * time.Minute} {
		m.refreshOneClaudeQuota(ctx, now.Add(offset))
	}
	if e.calls != 1 {
		t.Fatalf("other credential bypassed shared cooldown: %d reads", e.calls)
	}
	if errClose := kvstore.ShutdownLocal(); errClose != nil {
		t.Fatal(errClose)
	}
	if errConfig := kvstore.ConfigureLocal(true, path); errConfig != nil {
		t.Fatal(errConfig)
	}
	// A different credential after restart cannot bypass the pool gate.
	restarted, a, e2 := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return quotaTestWindows(now, 24*time.Hour), nil
	})
	restarted.mu.Lock()
	restarted.auths[a.ID].Metadata["access_token"] = "sk-ant-oat01-replacement"
	restarted.mu.Unlock()
	restarted.refreshOneClaudeQuota(ctx, now.Add(29*time.Minute))
	if e2.calls != 0 {
		t.Fatal("restart or new grant bypassed shared cooldown")
	}
	restarted.refreshOneClaudeQuota(ctx, now.Add(30*time.Minute))
	if e2.calls != 1 {
		t.Fatal("pool remained blocked after cooldown")
	}
}

func TestClaudeQuotaPoolHonorsLongerRetryAfter(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, _, e := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return nil, &ClaudeQuotaProbeError{Status: 429, RetryAt: now.Add(2 * time.Hour)}
	})
	m.refreshOneClaudeQuota(ctx, now)
	m.refreshOneClaudeQuota(ctx, now.Add(90*time.Minute))
	if e.calls != 1 {
		t.Fatal("ignored longer Retry-After")
	}
}

func TestClaudeQuotaNewFiveHourHeaderOverridesAbsentCapability(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m, a, _ := quotaProbeManager(t, func(context.Context, *Auth) (ClaudeQuotaSnapshot, error) {
		return quotaTestWindows(now, 24*time.Hour), nil
	})
	m.refreshOneClaudeQuota(ctx, now)
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.02")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	m.claudeQuota.observe(a, headers, now.Add(time.Minute))
	windows := m.claudeQuota.snapshot(a)
	if windows["5h"].Unlimited {
		t.Fatal("new hourly constraint ignored")
	}
	if _, _, blocked := claudeQuotaRank(windows, "claude-fable", now.Add(time.Minute)); !blocked {
		t.Fatal("hourly exhaustion did not block new bindings")
	}
}

func TestClaudeQuotaResetRankingOutlivesUsageSnapshot(t *testing.T) {
	now := time.Now()
	windows := quotaTestWindows(now, time.Hour)
	weekly := windows["7d"]
	weekly.Used = 1
	windows["7d"] = weekly
	if _, known, blocked := claudeQuotaRank(windows, "claude-sonnet", now.Add(16*time.Minute)); !known || blocked {
		t.Fatal("idle polling lost known reset or retained stale exhaustion")
	}
	if _, known, _ := claudeQuotaRank(windows, "claude-sonnet", now.Add(time.Hour)); known {
		t.Fatal("expired reset affected ranking")
	}
}

func TestClaudeQuotaFailureDiagnosticsAreSafe(t *testing.T) {
	r := &claudeQuotaRecord{Failures: 2, Next: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		probe  *ClaudeQuotaProbeError
		reason string
	}{
		{&ClaudeQuotaProbeError{Status: 429}, "rate_limited"},
		{&ClaudeQuotaProbeError{Status: 403}, "auth_rejected"},
		{&ClaudeQuotaProbeError{Status: 200, Reason: "invalid_window", Window: "five_hour"}, "invalid_window"},
		{&ClaudeQuotaProbeError{Reason: "transport_error"}, "transport_error"},
		{&ClaudeQuotaProbeError{Status: 500, Reason: "secret-token", Window: "secret-token"}, "http_error"},
		{nil, "unexpected_error"},
	} {
		fields := claudeQuotaFailureFields("test-account", r, tc.probe)
		if fields["failure_reason"] != tc.reason || fields["retry_at"] == nil || fields["failures"] != 2 {
			t.Fatal(fields)
		}
		if strings.Contains(fmt.Sprint(fields), "secret-token") {
			t.Fatal("unsafe error text entered logs")
		}
	}
}
