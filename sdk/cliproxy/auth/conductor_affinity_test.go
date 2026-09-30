package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func durableTestManager(t *testing.T, path string) (*Manager, []string) {
	t.Helper()
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}})
	t.Cleanup(s.Stop)
	m := NewManager(nil, s, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for _, id := range ids {
		if _, errRegister := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "model-one"}, {ID: "model-two"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, ids
}

func durableTestOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"stable-session"}}, Metadata: map[string]any{}}
}

func durableTestPick(m *Manager, model string, opts cliproxyexecutor.Options) (*Auth, error) {
	a, _, _, errPick := m.pickNextMixed(context.Background(), []string{"claude"}, model, opts, nil)
	return a, errPick
}

func TestDurableAffinityRestartModelsAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	opts := durableTestOptions()
	// Bind the second credential so a cold fill-first selection would expose a lost binding.
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[1]
	if a, errPick := durableTestPick(m, "model-one", opts); errPick != nil || a.ID != ids[1] {
		t.Fatalf("initial selection = %v, %v", a, errPick)
	}
	opts = durableTestOptions()
	restarted, _ := durableTestManager(t, path)
	for _, manager := range []*Manager{m, restarted} {
		manager.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}}))
		t.Cleanup(func() { manager.Selector().(*SessionAffinitySelector).Stop() })
		for _, model := range []string{"model-one", "model-two"} {
			a, errPick := durableTestPick(manager, model, opts)
			if errPick != nil || a.ID != ids[1] {
				t.Fatalf("restored %s = %v, %v", model, a, errPick)
			}
		}
	}
	// Read-only reuse must not rewrite the store.
	before, _ := os.Stat(path)
	if _, errPick := durableTestPick(restarted, "model-one", opts); errPick != nil {
		t.Fatal(errPick)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged binding rewrote the file")
	}
}

func TestDurableAffinityFailurePolicy(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name    string
		code    int
		scope   bool
		migrate bool
	}{
		{"transient", 503, false, false},
		{"unauthorized", 401, false, false},
		{"temporary rate limit", 429, false, false},
		{"confirmed quota", 429, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
			opts := durableTestOptions()
			if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
				t.Fatal(errPick)
			}
			cooldown := time.Hour
			m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Options: opts,
				Error: &Error{HTTPStatus: tc.code, Message: tc.name}, CredentialScope: tc.scope, RetryAfter: &cooldown})
			// The tried set must not allow ordinary errors to escape the durable owner.
			a, _, _, errPick := m.pickNextMixed(context.Background(), []string{"claude"}, "model-one", opts, map[string]struct{}{ids[0]: {}})
			if tc.migrate {
				if errPick != nil || a.ID != ids[1] {
					t.Fatalf("quota migration = %v, %v", a, errPick)
				}
				m.mu.Lock()
				m.auths[ids[0]].Quota = QuotaState{}
				m.auths[ids[0]].Unavailable = false
				m.auths[ids[0]].NextRetryAfter = time.Time{}
				m.auths[ids[0]].ModelStates = nil
				m.mu.Unlock()
				// A late successful result from A cannot reclaim a binding now owned by B.
				m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-two", Success: true, Options: opts})
				a, errPick = durableTestPick(m, "model-two", opts)
				if errPick != nil || a.ID != ids[1] {
					t.Fatalf("recovered account reclaimed session: %v, %v", a, errPick)
				}
			} else if errPick == nil || a != nil {
				t.Fatalf("temporary failure migrated: %v, %v", a, errPick)
			}
		})
	}
}

func TestDurableAffinityConcurrentFirstRequests(t *testing.T) {
	m, _ := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: m.durableAffinitySelector().file, Fallback: &RoundRobinSelector{}}))
	t.Cleanup(func() { m.Selector().(*SessionAffinitySelector).Stop() })
	var wg sync.WaitGroup
	owners := make(chan string, 24)
	for i := 0; i < cap(owners); i++ {
		wg.Go(func() {
			a, errPick := durableTestPick(m, "model-one", durableTestOptions())
			if errPick != nil {
				t.Error(errPick)
				return
			}
			owners <- a.ID
		})
	}
	wg.Wait()
	close(owners)
	var owner string
	for id := range owners {
		if owner != "" && owner != id {
			t.Fatal("concurrent requests chose different accounts")
		}
		owner = id
	}
}

func TestDurableAffinityFailsClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "write failure", "missing session", "removed owner", "unsupported model", "multiple providers"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if kind == "corrupt" {
				if errWrite := os.WriteFile(path, []byte("{"), 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			if kind == "write failure" {
				if errWrite := os.WriteFile(path, []byte("not a directory"), 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
				path = filepath.Join(path, "bindings.json")
			}
			m, ids := durableTestManager(t, path)
			opts := durableTestOptions()
			model := "model-one"
			providers := []string{"claude"}
			if kind == "removed owner" || kind == "unsupported model" {
				if _, errPick := durableTestPick(m, model, opts); errPick != nil {
					t.Fatal(errPick)
				}
				if kind == "removed owner" {
					m.mu.Lock()
					delete(m.auths, ids[0])
					m.mu.Unlock()
				} else {
					registry.GetGlobalRegistry().RegisterClient(ids[0], "claude", []*registry.ModelInfo{{ID: "model-two"}})
				}
			}
			if kind == "missing session" {
				opts = cliproxyexecutor.Options{}
			}
			if kind == "multiple providers" {
				providers = append(providers, "codex")
			}
			if a, _, _, errPick := m.pickNextMixed(context.Background(), providers, model, opts, nil); errPick == nil || a != nil {
				t.Fatalf("did not fail closed: %v, %v", a, errPick)
			}
		})
	}
}

type durableTestExecutor struct {
	schedulerTestExecutor
	attempts []string
	failure  error
}

func (e *durableTestExecutor) Execute(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.attempts = append(e.attempts, a.ID)
	if len(e.attempts) == 1 {
		return cliproxyexecutor.Response{}, e.failure
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *durableTestExecutor) CountTokens(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, a, r, o)
}

func (e *durableTestExecutor) ExecuteStream(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	response, errExec := e.Execute(ctx, a, r, o)
	if errExec != nil {
		return nil, errExec
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestDurableAffinityExecutionPaths(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, quota := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quota=%t", mode, quota), func(t *testing.T) {
				m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
				m.SetRetryConfig(2, time.Second, 0)
				cooldown := time.Hour
				e := &durableTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}, failure: streamQuotaError{
					customStatusError: customStatusError{code: 429, msg: "rate limit", retryAfter: &cooldown}, credentialScoped: quota,
				}}
				m.RegisterExecutor(e)
				opts := durableTestOptions()
				r := cliproxyexecutor.Request{Model: "model-one"}
				var errExec error
				switch mode {
				case "execute":
					_, errExec = m.Execute(context.Background(), []string{"claude"}, r, opts)
				case "count":
					_, errExec = m.ExecuteCount(context.Background(), []string{"claude"}, r, opts)
				case "stream":
					var stream *cliproxyexecutor.StreamResult
					stream, errExec = m.ExecuteStream(context.Background(), []string{"claude"}, r, opts)
					if stream != nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								errExec = chunk.Err
							}
						}
					}
				}
				if quota {
					if errExec != nil || len(e.attempts) != 2 || e.attempts[0] != ids[0] || e.attempts[1] != ids[1] {
						t.Fatalf("quota attempts=%v error=%v", e.attempts, errExec)
					}
				} else if errExec == nil || len(e.attempts) != 1 {
					t.Fatalf("temporary limit bypassed owner cooldown: attempts=%v error=%v", e.attempts, errExec)
				}
			})
		}
	}
}

func TestDurableAffinityTransportRetryUsesSameOwner(t *testing.T) {
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	m.SetRetryConfig(1, 0, 0)
	e := &durableTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}, failure: dialRefusedError()}
	m.RegisterExecutor(e)
	_, errExec := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "model-one"}, durableTestOptions())
	if errExec != nil || len(e.attempts) != 2 || e.attempts[0] != ids[0] || e.attempts[1] != ids[0] {
		t.Fatalf("same-account retry: attempts=%v error=%v", e.attempts, errExec)
	}
}

func TestDurableAffinityCallerIsolationAndChildInheritance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	for i, scope := range []string{"client-one", "client-two"} {
		opts := durableTestOptions()
		opts.Headers = http.Header{"X-Claude-Code-Session-Id": {"parent"}}
		opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = scope
		opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[i]
		if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
			t.Fatal(errPick)
		}
	}
	restarted, _ := durableTestManager(t, path)
	for i, scope := range []string{"client-one", "client-two"} {
		opts := durableTestOptions()
		opts.Headers = http.Header{"X-Claude-Code-Session-Id": {"parent"}, "X-Claude-Code-Agent-Id": {"child"}}
		opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = scope
		a, errPick := durableTestPick(restarted, "model-two", opts)
		if errPick != nil || a.ID != ids[i] {
			t.Fatalf("child inheritance for %s: %v, %v", scope, a, errPick)
		}
	}
}

func TestDurableAffinityNoReplacementRetainsOwner(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	opts := durableTestOptions()
	if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
		t.Fatal(errPick)
	}
	cooldown := time.Hour
	for _, id := range ids {
		m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "model-one", CredentialScope: true,
			Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &cooldown})
	}
	if _, errPick := durableTestPick(m, "model-one", opts); errPick == nil {
		t.Fatal("expected quota error")
	}
	store := loadDurableSessions(path)
	sessionID, _ := extractExplicitSessionIDs(opts.Headers, nil, opts.Metadata)
	if store.err != nil || store.bindings[durableSessionKey("claude", sessionID, opts)] != ids[0] {
		t.Fatalf("lost owner with no replacement: %+v", store)
	}
}

func TestDurableAffinityWriteFailureDoesNotDispatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "bindings.json")
	m, _ := durableTestManager(t, path)
	// Load an absent store, then prevent creation of its parent directory.
	if _, errStore := m.durableStore(m.durableAffinitySelector()); errStore != nil {
		t.Fatal(errStore)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, "state"), []byte("blocked"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	e := &durableTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}}
	m.RegisterExecutor(e)
	_, errExec := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "model-one"}, durableTestOptions())
	if errExec == nil || len(e.attempts) != 0 {
		t.Fatalf("dispatch despite persistence failure: %v, %v", e.attempts, errExec)
	}
}

func TestDurableAffinityBlocksCreditsFallback(t *testing.T) {
	withQuotaCooldownEnabled(t)
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: filepath.Join(t.TempDir(), "bindings.json")})
	defer s.Stop()
	m := NewManager(nil, s, nil)
	m.SetConfig(&internalconfig.Config{QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true}})
	e := &antigravityCreditsFallbackExecutor{}
	m.RegisterExecutor(e)
	const model = "claude-durable-credits-test"
	const id = "durable-credits-account"
	registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, errRegister := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "antigravity"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	stream, _ := m.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, durableTestOptions())
	if stream != nil {
		for range stream.Chunks {
		}
	}
	for _, credits := range e.streamCreditsRequested {
		if credits {
			t.Fatal("credits fallback bypassed durable affinity and cooldown")
		}
	}
	for _, code := range []int{429, 503} {
		if shouldAttemptAntigravityCreditsFallback(m, &Error{HTTPStatus: code}, []string{"antigravity"}) {
			t.Fatalf("status %d enabled unbound fallback", code)
		}
	}
}

func TestDurableAffinityAliasesRetainMigratedOwnerAfterRestart(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	conversation := cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":"conversation-one"}`), Metadata: map[string]any{}}
	cacheKey := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"cache-one","conversation":"conversation-one"}`), Metadata: map[string]any{}}
	for _, opts := range []cliproxyexecutor.Options{conversation, cacheKey} {
		if a, errPick := durableTestPick(m, "model-one", opts); errPick != nil || a.ID != ids[0] {
			t.Fatalf("initial alias selection = %v, %v", a, errPick)
		}
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Options: cacheKey,
		CredentialScope: true, Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &cooldown})
	if a, errPick := durableTestPick(m, "model-one", cacheKey); errPick != nil || a.ID != ids[1] {
		t.Fatalf("alias migration = %v, %v", a, errPick)
	}
	// Both accounts are healthy after restart. Neither alias may revive the old owner.
	restarted, _ := durableTestManager(t, path)
	cacheKeyOnly := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"cache-one"}`), Metadata: map[string]any{}}
	for _, opts := range []cliproxyexecutor.Options{conversation, cacheKey, cacheKeyOnly} {
		if a, errPick := durableTestPick(restarted, "model-two", opts); errPick != nil || a.ID != ids[1] {
			t.Fatalf("alias revived original owner: %v, %v", a, errPick)
		}
	}
}

func TestDurableAffinityConflictingAliasesFailClosed(t *testing.T) {
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	for i, body := range []string{`{"conversation":"conflicting"}`, `{"prompt_cache_key":"conflicting"}`} {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(body), Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: ids[i]}}
		if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
			t.Fatal(errPick)
		}
	}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"conflicting","conversation":"conflicting"}`), Metadata: map[string]any{}}
	if a, errPick := durableTestPick(m, "model-one", opts); errPick == nil || a != nil {
		t.Fatalf("conflicting owners were silently merged: %v, %v", a, errPick)
	}
}

func TestDurableAffinityChildMigrationDoesNotRebindParent(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	parent := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"parent"}}, Metadata: map[string]any{}}
	child := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"parent"}, "X-Claude-Code-Agent-Id": {"child"}}, Metadata: map[string]any{}}
	for _, opts := range []cliproxyexecutor.Options{parent, child} {
		if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
			t.Fatal(errPick)
		}
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", CredentialScope: true,
		Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &cooldown})
	if a, errPick := durableTestPick(m, "model-one", child); errPick != nil || a.ID != ids[1] {
		t.Fatalf("child migration = %v, %v", a, errPick)
	}
	restarted, _ := durableTestManager(t, path)
	for i, opts := range []cliproxyexecutor.Options{parent, child} {
		if a, errPick := durableTestPick(restarted, "model-two", opts); errPick != nil || a.ID != ids[i] {
			t.Fatalf("parent/child owners were merged: %v, %v", a, errPick)
		}
	}
}

func TestDurableAffinityModelQuotaScopeAndPersistence(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	store := NewFileCooldownStateStore(t.TempDir())
	m.SetCooldownStateStore(store)
	opts := durableTestOptions()
	if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
		t.Fatal(errPick)
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429, Message: "confirmed model quota"}, RetryAfter: &cooldown})
	// A late concurrent generic response cannot erase the already confirmed window.
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{HTTPStatus: 429, Message: "rate limit"}})
	restarted, _ := durableTestManager(t, path)
	restarted.SetCooldownStateStore(store)
	if errRestore := restarted.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatal(errRestore)
	}
	// A healthy sibling model must not authorize moving the same session.
	if a, errPick := durableTestPick(restarted, "model-two", opts); errPick != nil || a.ID != ids[0] {
		t.Fatalf("sibling selection = %v, %v", a, errPick)
	}
	if a, errPick := durableTestPick(restarted, "model-one", opts); errPick != nil || a.ID != ids[1] {
		t.Fatalf("persisted model quota migration = %v, %v", a, errPick)
	}
	// Migration moves the whole session, even with healthy accounts after restart.
	restartedAgain, _ := durableTestManager(t, path)
	if a, errPick := durableTestPick(restartedAgain, "model-two", opts); errPick != nil || a.ID != ids[1] {
		t.Fatalf("replacement did not retain whole-session ownership: %v, %v", a, errPick)
	}
}

func TestDurableAffinityExpiredModelQuotaKeepsOwner(t *testing.T) {
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	opts := durableTestOptions()
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[1]
	if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
		t.Fatal(errPick)
	}
	m.mu.Lock()
	m.auths[ids[1]].ModelStates = map[string]*ModelState{"model-one": {Quota: QuotaState{
		Exceeded: true, Reason: ErrorCodeModelQuota, NextRecoverAt: time.Now().Add(-time.Hour),
	}}}
	m.mu.Unlock()
	if a, errPick := durableTestPick(m, "model-one", durableTestOptions()); errPick != nil || a.ID != ids[1] {
		t.Fatalf("expired quota migrated owner: %v, %v", a, errPick)
	}
}

func TestDurableAffinityModelQuotaSurvivesLateSuccess(t *testing.T) {
	withQuotaCooldownEnabled(t)
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	opts := durableTestOptions()
	if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
		t.Fatal(errPick)
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429}, RetryAfter: &cooldown})
	// A request accepted before exhaustion can complete after the rejection.
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Success: true})
	if a, errPick := durableTestPick(m, "model-one", opts); errPick != nil || a.ID != ids[1] {
		t.Fatalf("late success erased confirmed exhaustion: %v, %v", a, errPick)
	}
}

func TestDurableAffinityModelQuotaDoesNotConfirmGenericDeadline(t *testing.T) {
	withQuotaCooldownEnabled(t)
	m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
	longCooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{HTTPStatus: 429}, RetryAfter: &longCooldown})
	shortCooldown := time.Minute
	before := time.Now()
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429}, RetryAfter: &shortCooldown})
	m.mu.RLock()
	state := m.auths[ids[0]].ModelStates["model-one"]
	confirmedUntil, retryAt := state.Quota.NextRecoverAt, state.NextRetryAfter
	m.mu.RUnlock()
	if confirmedUntil.Before(before.Add(shortCooldown)) || confirmedUntil.After(time.Now().Add(shortCooldown)) {
		t.Fatalf("confirmation inherited an unclassified deadline: %v", confirmedUntil)
	}
	if retryAt.Before(before.Add(59 * time.Minute)) {
		t.Fatalf("existing retry deadline was shortened: %v", retryAt)
	}
	// Reversing the response order must neither erase nor extend confirmation.
	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
		Error: &Error{HTTPStatus: 429}, RetryAfter: &longCooldown})
	m.mu.RLock()
	state = m.auths[ids[0]].ModelStates["model-one"]
	confirmationRetained := state.Quota.Reason == ErrorCodeModelQuota && state.Quota.NextRecoverAt.Equal(confirmedUntil)
	m.mu.RUnlock()
	if !confirmationRetained {
		t.Fatal("later generic response changed the confirmed window")
	}
}
