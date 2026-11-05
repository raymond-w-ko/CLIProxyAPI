package auth

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetFirstSelector spends Claude OAuth quota nearest its weekly reset.
// Manager attaches its shared cache before publishing the selector.
type SoonestResetFirstSelector struct {
	cache    atomic.Pointer[claudeQuotaCache]
	fallback RoundRobinSelector
}

func soonestResetSelector(s Selector) *SoonestResetFirstSelector {
	if affinity, ok := s.(*SessionAffinitySelector); ok {
		return soonestResetSelector(affinity.fallback)
	}
	selector, _ := s.(*SoonestResetFirstSelector)
	return selector
}

func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	available, errAvailable := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	cache := s.cache.Load()
	// A durable owner is already selected by the manager. Probe observations must
	// never revoke that owner or convert polling failures into migration permission.
	if pin, _ := opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey].(string); pin != "" || cache == nil {
		return s.fallback.Pick(ctx, provider, model, opts, available)
	}
	for _, a := range available {
		if !strings.EqualFold(a.Provider, "claude") {
			return s.fallback.Pick(ctx, provider, model, opts, available)
		}
	}

	type rankedAuth struct {
		reset time.Time
		known bool
	}
	ranks := make(map[string]rankedAuth, len(available))
	eligible := make([]*Auth, 0, len(available))
	models, _ := ctx.Value(soonestResetModelsKey{}).(map[string]string)
	for _, a := range available {
		if !isClaudeQuotaAuth(a) {
			eligible = append(eligible, a)
			continue
		}
		quotaModel := model
		if resolved := models[a.ID]; resolved != "" {
			quotaModel = resolved
		}
		reset, known, blocked := claudeQuotaRank(cache.snapshot(a), quotaModel, now)
		if blocked {
			continue
		}
		ranks[a.ID] = rankedAuth{reset, known}
		eligible = append(eligible, a)
	}
	var best, unknown []*Auth
	var earliest time.Time
	for _, a := range highestPriorityAuths(eligible) {
		rank := ranks[a.ID]
		if !rank.known {
			unknown = append(unknown, a)
			continue
		}
		if earliest.IsZero() || rank.reset.Before(earliest) {
			earliest = rank.reset
			best = []*Auth{a}
		} else if rank.reset.Equal(earliest) {
			best = append(best, a)
		}
	}
	if len(best) > 0 {
		return s.fallback.Pick(ctx, provider, model, opts, best)
	}
	if len(unknown) > 0 {
		return s.fallback.Pick(ctx, provider, model, opts, unknown)
	}
	return nil, &Error{Code: "quota_observation_unavailable", Message: fmt.Sprintf("all eligible %s credentials have exhausted observed quota", provider), HTTPStatus: 503}
}

func claudeQuotaRank(windows ClaudeQuotaSnapshot, model string, now time.Time) (time.Time, bool, bool) {
	relevant := []string{"5h", "7d"}
	lower := strings.ToLower(model)
	for _, name := range []string{"fable", "opus", "sonnet"} {
		if strings.Contains(lower, name) {
			key := name
			if name == "fable" {
				key = "7d_oi"
			}
			relevant = append(relevant, key)
		}
	}
	for _, name := range relevant {
		w := windows[name]
		if claudeQuotaWindowFresh(w, now, claudeQuotaRankAge) && !w.Unlimited && w.Used >= 1 {
			return time.Time{}, false, true
		}
	}
	weekly := windows["7d"]
	return weekly.Reset, claudeQuotaWindowFresh(weekly, now, claudeQuotaRankAge) && !weekly.Unlimited && weekly.Reset.After(now), false
}

// Keep model-scoped limits tied to each credential's resolved upstream model.
type soonestResetModelsKey struct{}

func (m *Manager) soonestResetContext(ctx context.Context, selector Selector, model string, auths []*Auth) context.Context {
	if soonestResetSelector(selector) == nil {
		return ctx
	}
	models := make(map[string]string, len(auths))
	for _, a := range auths {
		models[a.ID] = m.selectionModelForAuth(a, model)
	}
	return context.WithValue(ctx, soonestResetModelsKey{}, models)
}
