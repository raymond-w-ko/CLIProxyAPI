package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

func durableAffinityError(code, message string) error {
	return &Error{Code: code, Message: message, HTTPStatus: http.StatusServiceUnavailable}
}

func (m *Manager) durableAffinitySelector() *SessionAffinitySelector {
	s, _ := m.Selector().(*SessionAffinitySelector)
	if s == nil || s.file == "" {
		return nil
	}
	return s
}

func (m *Manager) validateDurableAffinityRouting(providers []string) error {
	if m.durableAffinitySelector() == nil {
		return nil
	}
	if m.HomeEnabled() || m.hasPluginScheduler() || len(providers) != 1 {
		return durableAffinityError("session_affinity_unsupported", "durable session affinity requires standalone routing to one provider without a scheduler plugin")
	}
	return nil
}

func (m *Manager) durableStore(s *SessionAffinitySelector) (*durableSessionStore, error) {
	path, errPath := filepath.Abs(s.file)
	if errPath != nil {
		return nil, durableAffinityError("session_affinity_storage", "cannot resolve session bindings path")
	}
	m.durableStoresMu.Lock()
	defer m.durableStoresMu.Unlock()
	if m.durableStores == nil {
		m.durableStores = make(map[string]*durableSessionStore)
	}
	store := m.durableStores[path]
	if store == nil {
		store = loadDurableSessions(path)
		m.durableStores[path] = store
		if store.err != nil {
			log.WithError(store.err).Error("load durable session affinity")
		}
	}
	return store, nil
}

func durableSessionKey(provider, sessionID string, opts cliproxyexecutor.Options) string {
	// Hash the tuple to avoid retaining raw client identities or ambiguous separators.
	data, _ := json.Marshal([]string{
		canonicalSchedulingProvider(provider),
		sessionMetadataString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey),
		cliproxysession.BoundSessionIdentity(sessionID),
	})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (m *Manager) durableOwnerExhaustion(authID, model string) (string, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[authID]
	if a == nil || a.Disabled || a.Status == StatusDisabled {
		return "", time.Time{}
	}
	now := time.Now()
	if a.Quota.Exceeded && a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(now) {
		return a.Quota.Reason, a.Quota.NextRecoverAt
	}
	state := existingModelState(a, m.selectionModelKeyForAuth(a, model))
	if state != nil && state.Quota.Exceeded && state.Quota.Reason == ErrorCodeModelQuota && state.Quota.NextRecoverAt.After(now) {
		return state.Quota.Reason, state.Quota.NextRecoverAt
	}
	return "", time.Time{}
}

type durableAffinityPick func(cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error)

func (m *Manager) pickWithDurableAffinity(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, pick durableAffinityPick) (*Auth, ProviderExecutor, string, error) {
	s := m.durableAffinitySelector()
	if s == nil {
		return pick(opts)
	}
	if errRouting := m.validateDurableAffinityRouting(providers); errRouting != nil {
		return nil, nil, "", errRouting
	}
	opts.EnsureMetadata()
	sessionID, parentID := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if sessionID == "" {
		return nil, nil, "", &Error{Code: "session_id_required", Message: "durable session affinity requires a stable explicit session ID", HTTPStatus: http.StatusBadRequest}
	}
	store, errStore := m.durableStore(s)
	if errStore != nil {
		return nil, nil, "", errStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.err != nil {
		return nil, nil, "", durableAffinityError("session_affinity_storage", "session bindings are unavailable; repair storage and restart")
	}
	key := durableSessionKey(providers[0], sessionID, opts)
	owner := store.owner(key)
	previousOwner := owner
	var aliases []string
	isFork, _ := opts.Metadata[cliproxyexecutor.IsForkMetadataKey].(bool)
	isSubagent := !isFork && isSubagentSession(sessionID, parentID)
	if parentID != "" {
		parentKey := durableSessionKey(providers[0], parentID, opts)
		parentOwner := store.owner(parentKey)
		if !isFork && !isSubagent {
			if owner != "" && parentOwner != "" && owner != parentOwner {
				return nil, nil, "", durableAffinityError("session_affinity_conflict", "session aliases have different durable owners")
			}
			aliases = append(aliases, parentKey)
		}
		if owner == "" && (!isSubagent || s.subagentAffinity) {
			owner = parentOwner
		}
	}
	quotaReason, quotaReset := m.durableOwnerExhaustion(owner, model)
	if owner != "" && quotaReason == "" {
		if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" && pinned != owner {
			return nil, nil, "", durableAffinityError("session_affinity_conflict", "requested credential conflicts with the durable session owner")
		}
		// Clone metadata so the temporary pin does not survive a quota-triggered
		// migration or overwrite an explicit caller pin in a subsequent round.
		metadata := make(map[string]any, len(opts.Metadata)+1)
		for k, v := range opts.Metadata {
			metadata[k] = v
		}
		metadata[cliproxyexecutor.PinnedAuthMetadataKey] = owner
		opts.Metadata = metadata
	}
	a, executor, provider, errPick := pick(opts)
	if errPick != nil || a == nil {
		return a, executor, provider, errPick
	}
	if errBind := store.bind(key, a.ID, aliases...); errBind != nil {
		logEntryWithRequestID(ctx).WithError(errBind).Error("persist durable session affinity")
		return nil, nil, "", durableAffinityError("session_affinity_storage", "cannot persist session owner; upstream request was not sent")
	}
	// Only report ownership after it has been persisted. Aliases use the same
	// binding hash, so logs remain correlatable across request identities/restarts.
	binding := store.root(key)
	if len(binding) > 12 {
		binding = binding[:12]
	}
	entry := logEntryWithRequestID(ctx).WithFields(log.Fields{
		"binding": binding, "auth": a.ID, "provider": provider, "model": model,
	})
	// The standard text formatter omits arbitrary fields; include these in the message too.
	detail := fmt.Sprintf("binding=%s auth=%q provider=%q model=%q", binding, a.ID, provider, model)
	switch {
	case owner != "" && owner != a.ID:
		reset := quotaReset.UTC().Format(time.RFC3339)
		entry.WithFields(log.Fields{"previous_auth": owner, "reason": quotaReason, "quota_reset": reset}).
			Infof("session-affinity: durable binding migrated | %s previous_auth=%q reason=%s quota_reset=%s", detail, owner, quotaReason, reset)
	case previousOwner == "":
		entry.WithField("inherited_from_auth", owner).
			Infof("session-affinity: durable binding created | %s inherited_from_auth=%q", detail, owner)
	default:
		entry.Debugf("session-affinity: durable binding retained | %s", detail)
	}
	return a, executor, provider, nil
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	a, executor, _, errPick := m.pickWithDurableAffinity(ctx, []string{provider}, model, opts, func(pickOpts cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error) {
		a, executor, errPick := m.pickNextLegacyUnbound(ctx, provider, model, pickOpts, tried)
		return a, executor, provider, errPick
	})
	return a, executor, errPick
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	return m.pickWithDurableAffinity(ctx, providers, model, opts, func(pickOpts cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error) {
		return m.pickNextMixedLegacyUnbound(ctx, providers, model, pickOpts, tried)
	})
}

// durableRetryOwner makes the existing retry budget and cooldown calculation
// consider the bound account rather than an unrelated, immediately ready account.
func (m *Manager) durableRetryOwner(providers []string, model string, opts cliproxyexecutor.Options) (string, error) {
	s := m.durableAffinitySelector()
	if s == nil {
		return "", nil
	}
	if errRouting := m.validateDurableAffinityRouting(providers); errRouting != nil {
		return "", errRouting
	}
	sessionID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	store, errStore := m.durableStore(s)
	if errStore != nil {
		return "", errStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.err != nil {
		return "", store.err
	}
	owner := store.owner(durableSessionKey(providers[0], sessionID, opts))
	if reason, _ := m.durableOwnerExhaustion(owner, model); reason != "" {
		return "", nil
	}
	return owner, nil
}
