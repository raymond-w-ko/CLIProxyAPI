package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func (m *Manager) durableOwnerExhausted(authID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[authID]
	return a != nil && !a.Disabled && a.Status != StatusDisabled &&
		a.Quota.Exceeded && a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(time.Now())
}

type durableAffinityPick func(cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error)

func (m *Manager) pickWithDurableAffinity(providers []string, opts cliproxyexecutor.Options, pick durableAffinityPick) (*Auth, ProviderExecutor, string, error) {
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
	if owner != "" && !m.durableOwnerExhausted(owner) {
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
		log.WithError(errBind).Error("persist durable session affinity")
		return nil, nil, "", durableAffinityError("session_affinity_storage", "cannot persist session owner; upstream request was not sent")
	}
	return a, executor, provider, nil
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	a, executor, _, errPick := m.pickWithDurableAffinity([]string{provider}, opts, func(pickOpts cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error) {
		a, executor, errPick := m.pickNextLegacyUnbound(ctx, provider, model, pickOpts, tried)
		return a, executor, provider, errPick
	})
	return a, executor, errPick
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	return m.pickWithDurableAffinity(providers, opts, func(pickOpts cliproxyexecutor.Options) (*Auth, ProviderExecutor, string, error) {
		return m.pickNextMixedLegacyUnbound(ctx, providers, model, pickOpts, tried)
	})
}

// durableRetryOwner makes the existing retry budget and cooldown calculation
// consider the bound account rather than an unrelated, immediately ready account.
func (m *Manager) durableRetryOwner(providers []string, opts cliproxyexecutor.Options) (string, error) {
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
	if m.durableOwnerExhausted(owner) {
		return "", nil
	}
	return owner, nil
}
