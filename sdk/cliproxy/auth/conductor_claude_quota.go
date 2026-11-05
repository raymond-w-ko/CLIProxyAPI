package auth

import (
	"context"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
)

// StartClaudeQuotaRefresh starts a single, cancelable worker. One request at a
// time and a one-second gap spread startup reads across credentials. It remains
// idle for other strategies and Home, including after config reload.
func (m *Manager) StartClaudeQuotaRefresh(ctx context.Context) {
	m.quotaWorkerMu.Lock()
	defer m.quotaWorkerMu.Unlock()
	if m.quotaCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.quotaCancel = cancel
	m.quotaDone = done
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			m.refreshOneClaudeQuota(ctx, time.Now())
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// StopClaudeQuotaRefresh joins the worker before the service closes local KV.
func (m *Manager) StopClaudeQuotaRefresh() {
	m.quotaWorkerMu.Lock()
	defer m.quotaWorkerMu.Unlock()
	if m.quotaCancel == nil {
		return
	}
	m.quotaCancel()
	<-m.quotaDone
	m.quotaCancel = nil
	m.quotaDone = nil
	for _, a := range m.List() {
		if isClaudeQuotaAuth(a) {
			m.claudeQuota.save(context.Background(), a, time.Now(), true)
		}
	}
}

func (m *Manager) refreshOneClaudeQuota(ctx context.Context, now time.Time) {
	if soonestResetSelector(m.Selector()) == nil || m.runtimeConfigSnapshot().Home.Enabled {
		return
	}
	auths := m.List()
	activeKeys := make(map[string]bool, len(auths))
	for _, a := range auths {
		if isClaudeQuotaAuth(a) {
			activeKeys[claudeQuotaKey(a)] = true
		}
	}
	m.claudeQuota.mu.Lock()
	for key := range m.claudeQuota.records {
		if !activeKeys[key] {
			delete(m.claudeQuota.records, key)
		}
	}
	m.claudeQuota.mu.Unlock()
	var candidate *Auth
	var fetcher ClaudeQuotaFetcher
	for _, a := range auths {
		if !isClaudeQuotaAuth(a) || a.Disabled {
			continue
		}
		m.claudeQuota.load(ctx, a)
		m.claudeQuota.save(ctx, a, now, false)
		if candidate != nil {
			continue
		}
		executor, ok := m.Executor(executorKeyFromAuth(a))
		if !ok {
			continue
		}
		f, ok := executor.(ClaudeQuotaFetcher)
		if !ok {
			continue
		}
		m.claudeQuota.mu.Lock()
		r := m.claudeQuota.recordLocked(claudeQuotaKey(a))
		if authHasUnchangedRejectedRefreshToken(a) && !r.Paused {
			r.Paused = true
			r.markDirty()
		}
		due := claudeQuotaProbeDue(r, now)
		if due {
			r.Next = now.Add(time.Minute)
			r.markDirty()
		}
		m.claudeQuota.mu.Unlock()
		if due {
			candidate = a
			fetcher = f
		}
	}
	if candidate == nil || ctx.Err() != nil {
		return
	}
	// Save the attempt gate before sending so restart cannot hammer the endpoint.
	m.claudeQuota.save(ctx, candidate, now, true)
	probeCtx := ctx
	if rt := m.roundTripperFor(candidate); rt != nil {
		probeCtx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	started := now
	windows, errFetch := fetcher.FetchClaudeQuota(probeCtx, candidate)
	if ctx.Err() != nil {
		return
	}
	var probeErr *ClaudeQuotaProbeError
	errors.As(errFetch, &probeErr)
	if probeErr != nil && probeErr.Status == 401 && !candidate.HasValidAccessToken(now) {
		// Refresh an expired token through the existing single-flight path. A
		// quota-endpoint rejection of a valid token is not an inference failure.
		if !authHasUnchangedRejectedRefreshToken(candidate) {
			refreshed, errRefresh := m.refreshAuthForRequestAtEpoch(ctx, candidate.ID, "", candidate.RegistrationEpoch)
			if errRefresh == nil && refreshed != nil && claudeQuotaKey(refreshed) != claudeQuotaKey(candidate) {
				// The expired token's rejection says nothing about the new grant.
				m.claudeQuota.mu.Lock()
				next := m.claudeQuota.recordLocked(claudeQuotaKey(refreshed))
				next.Next = now.Add(time.Minute)
				next.markDirty()
				m.claudeQuota.mu.Unlock()
				m.claudeQuota.save(ctx, refreshed, time.Now(), true)
				return
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	current := m.auths[candidate.ID]
	valid := current != nil && current.RegistrationEpoch == candidate.RegistrationEpoch && claudeQuotaKey(current) == claudeQuotaKey(candidate)
	terminal := current != nil && authHasUnchangedRejectedRefreshToken(current)
	if valid {
		m.claudeQuota.mu.Lock()
		r := m.claudeQuota.recordLocked(claudeQuotaKey(candidate))
		if errFetch == nil {
			// Endpoint data is dated at request start. A newer passive observation wins.
			applyClaudeQuotaFull(r.Windows, windows, started)
			r.Failures = 0
			r.AuthFailures = 0
		} else {
			r.Failures++
			delay := time.Minute
			for n := 1; n < r.Failures && delay < 30*time.Minute; n++ {
				delay *= 2
			}
			if delay > 30*time.Minute {
				delay = 30 * time.Minute
			}
			r.Next = now.Add(delay)
			if probeErr != nil {
				if probeErr.Status == 401 || probeErr.Status == 403 {
					r.AuthFailures++
					r.Paused = r.AuthFailures >= 3 || terminal
				}
				if probeErr.RetryAt.After(r.Next) {
					r.Next = probeErr.RetryAt
				}
			}
			log.WithFields(log.Fields{"auth_id": candidate.ID, "paused": r.Paused}).Warn("Claude quota refresh failed; inference ownership is unchanged")
		}
		r.markDirty()
		m.claudeQuota.mu.Unlock()
	}
	m.mu.RUnlock()
	if valid {
		m.claudeQuota.save(ctx, candidate, time.Now(), true)
	}
}
