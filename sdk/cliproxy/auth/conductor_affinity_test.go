package auth

import (
	"context"
	"errors"
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
	return durableTestManagerWithRebindOn(t, path, "")
}

func durableTestManagerWithRebindOn(t *testing.T, path, rebindOn string) (*Manager, []string) {
	t.Helper()
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}, RebindOn: rebindOn})
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
	for _, rebindOn := range []string{SessionAffinityRebindOnAny, SessionAffinityRebindOnQuota} {
		t.Run(rebindOn, func(t *testing.T) {
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
					m, ids := durableTestManagerWithRebindOn(t, filepath.Join(t.TempDir(), "bindings.json"), rebindOn)
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
		})
	}
}

func TestDurableAffinityTerminalUnauthorizedKeepsOwner(t *testing.T) {
	m, executor, primary, _, model := newUnauthorizedRefreshFixture(t, false)
	path := filepath.Join(t.TempDir(), "bindings.json")
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}})
	t.Cleanup(selector.Stop)
	m.SetSelector(selector)
	executor.refreshErr = errors.New("invalid_grant: refresh token invalid")
	opts := durableTestOptions()
	for i := 0; i < 2; i++ {
		if _, errExecute := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, opts); errExecute == nil {
			t.Fatal("terminal unauthorized owner unexpectedly succeeded through another account")
		}
	}
	if calls := executor.ExecuteCalls(); len(calls) != 1 || calls[0] != primary.ID {
		t.Fatalf("execution bypassed terminal owner: %v", calls)
	}
	if calls := executor.RefreshCalls(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	owner, ok := m.GetByID(primary.ID)
	if !ok || !hasUnauthorizedAuthFailure(owner) {
		t.Fatal("owner did not retain terminal unauthorized state")
	}
	store := loadDurableSessions(path)
	if store.err != nil {
		t.Fatal(store.err)
	}
	sessionID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if got := store.owner(durableSessionKey("codex", sessionID, opts)); got != primary.ID {
		t.Fatalf("persisted owner = %q, want %q", got, primary.ID)
	}
}

func TestDurableAffinityRejectedUnexpiredTokenKeepsOwner(t *testing.T) {
	m, executor, primary, _, model := newUnauthorizedRefreshFixture(t, false)
	path := filepath.Join(t.TempDir(), "bindings.json")
	installSelector := func() {
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}})
		t.Cleanup(selector.Stop)
		m.SetSelector(selector)
	}
	installSelector()
	primary.Metadata["expired"] = time.Now().Add(8 * 24 * time.Hour).Format(time.RFC3339)
	if _, errUpdate := m.Update(context.Background(), primary); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	executor.refreshErr = errors.New("refresh upstream unavailable")
	opts := durableTestOptions()
	request := cliproxyexecutor.Request{Model: model}
	if _, errExecute := m.Execute(context.Background(), []string{"codex"}, request, opts); errExecute == nil {
		t.Fatal("rejected owner unexpectedly succeeded through another account")
	}
	owner, ok := m.GetByID(primary.ID)
	if !ok || owner.RejectedAccessToken != "stale-access-token" || owner.NextRefreshAfter.IsZero() {
		t.Fatal("rejected unexpired token did not schedule refresh")
	}
	// Reload the persisted binding while the owner's refresh remains unavailable.
	installSelector()
	if _, errExecute := m.Execute(context.Background(), []string{"codex"}, request, opts); errExecute == nil {
		t.Fatal("binding reload bypassed the rejected owner")
	}
	executor.mu.Lock()
	executor.refreshErr = nil
	executor.mu.Unlock()
	loop := newAuthAutoRefreshLoop(m, 5*time.Second, 1)
	loop.handleDueAuth(context.Background(), owner.NextRefreshAfter.Add(time.Second), primary.ID)
	select {
	case job := <-loop.jobs:
		if !m.beginRefreshJob(context.Background(), job) {
			t.Fatal("background refresh did not start")
		}
		if _, errRefresh := m.refreshAuthForRequestAtEpoch(context.Background(), job.id, "", job.registrationEpoch); errRefresh != nil {
			t.Fatal(errRefresh)
		}
		m.finishRefreshJob(job, time.Time{}, false)
	default:
		t.Fatal("rejected token did not enqueue background refresh")
	}
	response, errExecute := m.Execute(context.Background(), []string{"codex"}, request, opts)
	if errExecute != nil || string(response.Payload) != primary.ID+":fresh-access-token" {
		t.Fatalf("recovered owner response = %s, %v", response.Payload, errExecute)
	}
	for _, id := range executor.ExecuteCalls() {
		if id != primary.ID {
			t.Fatalf("non-quota failure migrated to %s", id)
		}
	}
	store := loadDurableSessions(path)
	sessionID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if store.err != nil || store.owner(durableSessionKey("codex", sessionID, opts)) != primary.ID {
		t.Fatal("persisted owner changed during token recovery")
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
	for _, rebindOn := range []string{SessionAffinityRebindOnAny, SessionAffinityRebindOnQuota} {
		t.Run(rebindOn, func(t *testing.T) {
			for _, mode := range []string{"execute", "count", "stream"} {
				for _, quota := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/quota=%t", mode, quota), func(t *testing.T) {
						m, ids := durableTestManagerWithRebindOn(t, filepath.Join(t.TempDir(), "bindings.json"), rebindOn)
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
		})
	}
}

func TestDurableAffinityTransportRetryUsesSameOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"dial refused", dialRefusedError()},
		{"http2 force closed", errors.New("http2: client connection force closed via ClientConn.Close")},
		{"http2 conn closed", errors.New("http2: client conn is closed")},
		{"configured TLS handshake timeout", cliproxyexecutor.NewConfiguredTLSHandshakeTimeoutError(context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
			m.SetRetryConfig(1, 0, 0)
			e := &durableTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}, failure: tc.err}
			m.RegisterExecutor(e)
			_, errExec := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "model-one"}, durableTestOptions())
			if errExec != nil || len(e.attempts) != 2 || e.attempts[0] != ids[0] || e.attempts[1] != ids[0] {
				t.Fatalf("same-account retry: attempts=%v error=%v", e.attempts, errExec)
			}
		})
	}
}

type durableModelQuotaError struct{ customStatusError }

func (durableModelQuotaError) IsModelQuotaExhausted() bool { return true }

func TestDurableAffinityCanceledStreamQuotaPolicy(t *testing.T) {
	withQuotaCooldownEnabled(t)
	cooldown := time.Hour
	for _, tc := range []struct {
		name    string
		err     error
		migrate bool
	}{
		{"credential quota", streamQuotaError{customStatusError: customStatusError{code: 429, msg: "quota", retryAfter: &cooldown}, credentialScoped: true}, true},
		{"model quota", durableModelQuotaError{customStatusError{code: 429, msg: "model quota", retryAfter: &cooldown}}, true},
		{"generic 429", customStatusError{code: 429, msg: "rate limit", retryAfter: &cooldown}, false},
		{"temporary error", customStatusError{code: 503, msg: "unavailable", retryAfter: &cooldown}, false},
		{"client cancellation", context.Canceled, false},
	} {
		for _, bootstrap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bootstrap=%t", tc.name, bootstrap), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "bindings.json")
				m, ids := durableTestManager(t, path)
				m.SetRetryConfig(0, 0, 0)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				attempts := 0
				m.RegisterExecutor(&customStreamMockExecutor{
					identifier: "claude",
					streamFn: func(_ context.Context, selected *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						attempts++
						if selected.ID != ids[0] {
							t.Fatalf("canceled request dispatched to replacement %s", selected.ID)
						}
						cancel()
						if !bootstrap {
							return nil, tc.err
						}
						chunks := make(chan cliproxyexecutor.StreamChunk, 1)
						chunks <- cliproxyexecutor.StreamChunk{Err: tc.err}
						close(chunks)
						return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
					},
				})
				opts := durableTestOptions()
				_, errStream := m.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: "model-one"}, opts)
				if !errors.Is(errStream, context.Canceled) || attempts != 1 {
					t.Fatalf("canceled stream: attempts=%d error=%v", attempts, errStream)
				}
				sessionID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
				key := durableSessionKey("claude", sessionID, opts)
				store := loadDurableSessions(path)
				if store.err != nil || store.owner(key) != ids[0] {
					t.Fatalf("cancellation changed persisted owner: owner=%s error=%v", store.owner(key), store.err)
				}
				selected, errPick := durableTestPick(m, "model-one", durableTestOptions())
				wantOwner := ids[0]
				if tc.migrate {
					wantOwner = ids[1]
					if errPick != nil || selected == nil || selected.ID != wantOwner {
						t.Fatalf("confirmed quota did not permit next-request migration: %v, %v", selected, errPick)
					}
				} else if selected != nil && selected.ID != wantOwner {
					t.Fatalf("unconfirmed failure migrated: %v, %v", selected, errPick)
				}
				// With fresh credentials, a restart must retain the selected owner.
				restarted, _ := durableTestManager(t, path)
				selected, errPick = durableTestPick(restarted, "model-one", durableTestOptions())
				if errPick != nil || selected == nil || selected.ID != wantOwner {
					t.Fatalf("restart owner=%v error=%v, want %s", selected, errPick, wantOwner)
				}
			})
		}
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

func TestDurableAffinityGuardianRestartAndQuotaPolicy(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		migrate bool
	}{
		{name: "unauthorized", status: 401},
		{name: "generic rate limit", status: 429},
		{name: "confirmed model quota", status: 429, code: ErrorCodeModelQuota, migrate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			m, ids := durableTestManager(t, path)
			parent := cliproxyexecutor.Options{
				Headers: http.Header{"Session_id": {"guardian-parent"}},
				Metadata: map[string]any{
					cliproxyexecutor.CallerScopeMetadataKey: "guardian-caller",
					cliproxyexecutor.PinnedAuthMetadataKey:  ids[1],
				},
			}
			if _, errPick := durableTestPick(m, "model-one", parent); errPick != nil {
				t.Fatal(errPick)
			}
			// The guardian's first request must inherit across models with a cold memory cache.
			restarted, _ := durableTestManager(t, path)
			child := guardianTestOptions("guardian-child", "guardian-parent")
			child.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "guardian-caller"
			selected, errPick := durableTestPick(restarted, "model-two", child)
			if errPick != nil || selected.ID != ids[1] {
				t.Fatalf("guardian inheritance = %v, %v", selected, errPick)
			}
			cooldown := time.Hour
			restarted.MarkResult(context.Background(), Result{
				AuthID: ids[1], Provider: "claude", Model: "model-two", Options: child,
				Error: &Error{HTTPStatus: tc.status, Code: tc.code}, RetryAfter: &cooldown,
			})
			selected, _, _, errPick = restarted.pickNextMixed(context.Background(), []string{"claude"}, "model-two", child, map[string]struct{}{ids[1]: {}})
			want := ids[1]
			if tc.migrate {
				want = ids[0]
				if errPick != nil || selected.ID != want {
					t.Fatalf("guardian quota migration = %v, %v", selected, errPick)
				}
			} else if errPick == nil || selected != nil {
				t.Fatalf("guardian bypassed unavailable owner: %v, %v", selected, errPick)
			}
			// A later parent selection cannot replace an existing child's durable owner.
			reloaded, _ := durableTestManager(t, path)
			if selected, errPick = durableTestPick(reloaded, "model-one", parent); errPick != nil || selected.ID != ids[1] {
				t.Fatalf("parent owner = %v, %v", selected, errPick)
			}
			if selected, errPick = durableTestPick(reloaded, "model-one", child); errPick != nil || selected.ID != want {
				t.Fatalf("restored guardian owner = %v, %v; want %s", selected, errPick, want)
			}
		})
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

type durableMetadataBlockingStore struct {
	callbackAuthStore
	once    sync.Once
	entered chan struct{}
}

func (s *durableMetadataBlockingStore) Save(ctx context.Context, _ *Auth) (string, error) {
	var err error
	s.once.Do(func() {
		close(s.entered)
		<-ctx.Done()
		err = ctx.Err()
	})
	return "", err
}

func TestDurableAffinityMetadataSavePreservesConcurrentQuota(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%t", confirmed), func(t *testing.T) {
			m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
			opts := durableTestOptions()
			owner, errPick := durableTestPick(m, "model-one", opts)
			if errPick != nil {
				t.Fatal(errPick)
			}
			quotaErr := &Error{HTTPStatus: 429, Message: "rate limit"}
			if confirmed {
				quotaErr.Code = ErrorCodeModelQuota
			}
			cooldown := time.Hour
			store := &durableMetadataBlockingStore{entered: make(chan struct{})}
			m.SetStore(store)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan [2]bool, 1)
			go func() {
				ok, preempted := m.PersistMetadataEntry(ctx, owner.ID, owner.RegistrationEpoch, "capability_hint", true, nil)
				done <- [2]bool{ok, preempted}
			}()
			select {
			case <-store.entered:
			case <-ctx.Done():
				t.Fatal("metadata save did not enter store")
			}
			// A foreground quota result must preempt the blocked metadata save.
			m.MarkResult(ctx, Result{AuthID: owner.ID, Provider: "claude", Model: "model-one",
				Error: quotaErr, RetryAfter: &cooldown, CredentialVersion: owner.CredentialVersion, RegistrationEpoch: owner.RegistrationEpoch})
			if result := <-done; result != [2]bool{false, true} {
				t.Fatalf("metadata save was not preempted: %v", result)
			}
			ok, preempted := m.PersistMetadataEntry(ctx, owner.ID, owner.RegistrationEpoch, "capability_hint", true, nil)
			m.SetStore(nil)
			if !ok || preempted {
				t.Fatalf("metadata retry: ok=%t preempted=%t", ok, preempted)
			}
			current, _ := m.GetByID(owner.ID)
			if current.Metadata["capability_hint"] != true || current.RegistrationEpoch != owner.RegistrationEpoch || current.CredentialVersion != owner.CredentialVersion {
				t.Fatal("metadata save lost its value or changed credential identity")
			}
			if sibling, errSibling := durableTestPick(m, "model-two", opts); errSibling != nil || sibling.ID != ids[0] {
				t.Fatalf("metadata save changed sibling owner: %v, %v", sibling, errSibling)
			}
			selected, errPick := durableTestPick(m, "model-one", opts)
			if confirmed {
				if errPick != nil || selected == nil || selected.ID != ids[1] {
					t.Fatalf("metadata save lost confirmed model quota: %v, %v", selected, errPick)
				}
			} else if errPick == nil || selected != nil {
				t.Fatalf("metadata save bypassed generic cooldown: %v, %v", selected, errPick)
			}
		})
	}
}

func TestDurableAffinityFileSnapshotPreservesMigrationPolicy(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		credential bool
		migrate    bool
	}{
		{"generic rate limit", 429, "", false, false},
		{"model quota", 429, ErrorCodeModelQuota, false, true},
		{"credential quota", 429, "", true, true},
		{"configured 401 cooldown", 401, ErrorCodeForceCooldown, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindingPath := filepath.Join(t.TempDir(), "bindings.json")
			m, ids := durableTestManager(t, bindingPath)
			opts := durableTestOptions()
			owner, errPick := durableTestPick(m, "model-one", opts)
			if errPick != nil {
				t.Fatal(errPick)
			}
			// Capture a healthy file view before the live request records a failure.
			snapshot := owner.Clone()
			path := filepath.Join(t.TempDir(), "auth.json")
			data := []byte(`{"type":"claude"}`)
			if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			snapshot.Attributes = map[string]string{AttributePath: path, "excluded_models": "new-model"}
			snapshot.BindFileSnapshot(path, data)
			cooldown := time.Hour
			m.MarkResult(context.Background(), Result{
				AuthID: owner.ID, Provider: "claude", Model: "model-one",
				Error:           &Error{HTTPStatus: tc.status, Code: tc.code, Message: tc.name},
				CredentialScope: tc.credential, RetryAfter: &cooldown,
				CredentialVersion: owner.CredentialVersion, RegistrationEpoch: owner.RegistrationEpoch,
			})
			if _, errUpdate := m.Update(WithSkipPersist(context.Background()), snapshot); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			current, _ := m.GetByID(owner.ID)
			if current.Attributes["excluded_models"] != "new-model" {
				t.Fatal("file snapshot was not applied")
			}
			selected, errPick := durableTestPick(m, "model-one", opts)
			wantOwner := ids[0]
			if tc.migrate {
				wantOwner = ids[1]
				if errPick != nil || selected == nil || selected.ID != wantOwner {
					t.Fatalf("snapshot lost confirmed quota migration: %v, %v", selected, errPick)
				}
			} else if errPick == nil || selected != nil {
				t.Fatalf("snapshot bypassed owner cooldown: %v, %v", selected, errPick)
			}
			// A restart and a different model must use the last saved owner.
			restarted, _ := durableTestManager(t, bindingPath)
			selected, errPick = durableTestPick(restarted, "model-two", opts)
			if errPick != nil || selected == nil || selected.ID != wantOwner {
				t.Fatalf("restored owner = %v, %v; want %s", selected, errPick, wantOwner)
			}
		})
	}
}

func TestDurableAffinityReloginPreservesOwnership(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		credential bool
		migrate    bool
	}{
		{"refresh failure", 0, "", false, false},
		{"generic rate limit", 429, "", false, false},
		{"model quota", 429, ErrorCodeModelQuota, false, true},
		{"credential quota", 429, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindingPath := filepath.Join(t.TempDir(), "bindings.json")
			m, ids := durableTestManager(t, bindingPath)
			ctx := WithSkipPersist(context.Background())
			authPath := filepath.Join(t.TempDir(), "claude.json")
			updateToken := func(token string, expiry time.Time) {
				t.Helper()
				snapshot := reloginSnapshot(t, authPath, token, expiry)
				snapshot.ID = ids[0]
				if _, errUpdate := m.Update(ctx, snapshot); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			opts := durableTestOptions()
			if owner, errPick := durableTestPick(m, "model-one", opts); errPick != nil || owner.ID != ids[0] {
				t.Fatalf("initial owner = %v, %v", owner, errPick)
			}
			updateToken("old", time.Now().Add(-time.Hour))
			if tc.status != 0 {
				cooldown := time.Hour
				current, _ := m.GetByID(ids[0])
				m.MarkResult(ctx, Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
					Error: &Error{HTTPStatus: tc.status, Code: tc.code}, CredentialScope: tc.credential, RetryAfter: &cooldown,
					CredentialVersion: current.CredentialVersion, RegistrationEpoch: current.RegistrationEpoch})
			}
			m.RegisterExecutor(&unauthorizedRefreshExecutor{id: "claude", refreshErr: errors.New("invalid_grant: refresh token invalid")})
			m.refreshAuth(ctx, ids[0])
			if !tc.migrate {
				if owner, errPick := durableTestPick(m, "model-one", opts); errPick == nil || owner != nil {
					t.Fatalf("refresh failure bypassed owner: %v, %v", owner, errPick)
				}
			}
			updateToken("new", time.Now().Add(8*time.Hour))
			recovered, _ := m.GetByID(ids[0])
			if recovered.RefreshFailures != 0 || !recovered.NextRefreshAfter.IsZero() {
				t.Fatal("re-login retained refresh backoff")
			}
			wantOwner := ids[0]
			if tc.migrate {
				wantOwner = ids[1]
			}
			owner, errPick := durableTestPick(m, "model-one", opts)
			if tc.status != 0 && !tc.migrate {
				if errPick == nil || owner != nil {
					t.Fatalf("re-login bypassed generic cooldown: %v, %v", owner, errPick)
				}
			} else if errPick != nil || owner == nil || owner.ID != wantOwner {
				t.Fatalf("owner after re-login = %v, %v; want %s", owner, errPick, wantOwner)
			}
			// Another login must not reclaim a session that already migrated.
			updateToken("newer", time.Now().Add(8*time.Hour))
			restarted, _ := durableTestManager(t, bindingPath)
			for _, manager := range []*Manager{m, restarted} {
				owner, errPick = durableTestPick(manager, "model-two", opts)
				if errPick != nil || owner == nil || owner.ID != wantOwner {
					t.Fatalf("saved owner on another model = %v, %v; want %s", owner, errPick, wantOwner)
				}
			}
		})
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
	for _, dispatch := range []string{"unstamped", "before quota", "after quota"} {
		for _, terminalUnauthorized := range []bool{false, true} {
			t.Run(fmt.Sprintf("dispatch=%s/terminalUnauthorized=%t", dispatch, terminalUnauthorized), func(t *testing.T) {
				m, ids := durableTestManager(t, filepath.Join(t.TempDir(), "bindings.json"))
				opts := durableTestOptions()
				if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
					t.Fatal(errPick)
				}
				var successSeq uint64
				if dispatch == "before quota" {
					successSeq = m.nextExecutionSeq()
				}
				cooldown := time.Hour
				m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
					Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429}, RetryAfter: &cooldown, ExecutionSeq: m.nextExecutionSeq()})
				if dispatch == "after quota" {
					successSeq = m.nextExecutionSeq()
				}
				if terminalUnauthorized {
					m.mu.Lock()
					owner := m.auths[ids[0]]
					owner.Unavailable = true
					owner.Status = StatusError
					owner.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized}
					owner.NextRefreshAfter = time.Time{}
					owner.NextRetryAfter = time.Time{}
					m.mu.Unlock()
				}
				// Explicit model exhaustion keeps its deadline even when a later dispatch
				// succeeds. The generic cooldown watermark must not weaken this rule.
				m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Success: true, ExecutionSeq: successSeq})
				if terminalUnauthorized {
					owner, _ := m.GetByID(ids[0])
					if !hasUnauthorizedAuthFailure(owner) {
						t.Fatal("late success revived terminal unauthorized credential")
					}
				}
				if a, errPick := durableTestPick(m, "model-one", opts); errPick != nil || a.ID != ids[1] {
					t.Fatalf("late success erased confirmed exhaustion: %v, %v", a, errPick)
				}
			})
		}
	}
}

func TestDurableAffinityLayeredCooldownRestoresModelPolicy(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%t", confirmed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			m, ids := durableTestManager(t, path)
			store := NewFileCooldownStateStore(t.TempDir())
			m.SetCooldownStateStore(store)
			opts := durableTestOptions()
			if _, errPick := durableTestPick(m, "model-one", opts); errPick != nil {
				t.Fatal(errPick)
			}
			modelError := &Error{HTTPStatus: 429, Message: "model limit"}
			if confirmed {
				modelError.Code = ErrorCodeModelQuota
			}
			modelDelay := time.Hour
			m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
				Error: modelError, RetryAfter: &modelDelay})
			owner, _ := m.GetByID(ids[0])
			modelDeadline := owner.ModelStates["model-one"].Quota.NextRecoverAt
			accountDelay := time.Minute
			m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-two",
				Error: &Error{HTTPStatus: 429}, CredentialScope: true, RetryAfter: &accountDelay})

			restarted, _ := durableTestManager(t, path)
			restarted.SetCooldownStateStore(store)
			if errRestore := restarted.RestoreCooldownStates(context.Background()); errRestore != nil {
				t.Fatal(errRestore)
			}
			// Expire only the account layer without waiting. A later generic failure
			// must recover the persisted model layer without inventing exhaustion.
			restarted.mu.Lock()
			restarted.auths[ids[0]].Quota.NextRecoverAt = time.Now().Add(-time.Second)
			restarted.mu.Unlock()
			genericDelay := 2 * time.Hour
			restarted.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one",
				Error: &Error{HTTPStatus: 429}, RetryAfter: &genericDelay})
			owner, _ = restarted.GetByID(ids[0])
			state := owner.ModelStates["model-one"]
			if confirmed && (state.Quota.Reason != ErrorCodeModelQuota || !state.Quota.NextRecoverAt.Equal(modelDeadline)) {
				t.Fatalf("model confirmation changed after restoring layers: %+v", state.Quota)
			}
			selected, errPick := durableTestPick(restarted, "model-one", opts)
			if confirmed {
				if errPick != nil || selected.ID != ids[1] {
					t.Fatalf("confirmed model quota did not authorize migration: %v, %v", selected, errPick)
				}
			} else if errPick == nil {
				t.Fatalf("generic model cooldown unexpectedly selected %v", selected)
			}
			// A further restart proves only confirmed exhaustion changed ownership.
			final, _ := durableTestManager(t, path)
			wantOwner := ids[0]
			if confirmed {
				wantOwner = ids[1]
			}
			if selected, errPick := durableTestPick(final, "model-two", opts); errPick != nil || selected.ID != wantOwner {
				t.Fatalf("persisted owner = %v, %v; want %s", selected, errPick, wantOwner)
			}
		})
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

func TestDurableAffinityStaleQuotaAfterCredentialUpdateKeepsOwner(t *testing.T) {
	withQuotaCooldownEnabled(t)
	path := filepath.Join(t.TempDir(), "bindings.json")
	m, ids := durableTestManager(t, path)
	opts := durableTestOptions()
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[1]
	before, errPick := durableTestPick(m, "model-one", opts)
	if errPick != nil {
		t.Fatal(errPick)
	}
	updated := before.Clone()
	updated.Metadata = map[string]any{"access_token": "replacement-test-token"}
	current, errUpdate := m.Update(context.Background(), updated)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if current.CredentialVersion <= before.CredentialVersion {
		t.Fatal("credential replacement did not advance the version")
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: before.ID, Provider: "claude", Model: "model-one",
		CredentialVersion: before.CredentialVersion, RegistrationEpoch: before.RegistrationEpoch,
		Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429}, RetryAfter: &cooldown})
	if a, errPick := durableTestPick(m, "model-one", durableTestOptions()); errPick != nil || a.ID != ids[1] {
		t.Fatalf("stale quota changed durable ownership: %v, %v", a, errPick)
	}
	restarted, _ := durableTestManager(t, path)
	if a, errPick := durableTestPick(restarted, "model-two", durableTestOptions()); errPick != nil || a.ID != ids[1] {
		t.Fatalf("stale quota changed persisted ownership: %v, %v", a, errPick)
	}
}

func TestDurableAffinityResultTokenAfterCredentialUpdate(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name            string
		status          int
		code            string
		credentialScope bool
		replaceToken    bool
		migrate         bool
	}{
		{name: "same token unauthorized", status: 401},
		{name: "same token generic rate limit", status: 429},
		{name: "same token model quota", status: 429, code: ErrorCodeModelQuota, migrate: true},
		{name: "same token credential quota", status: 429, credentialScope: true, migrate: true},
		{name: "replaced token model quota", status: 429, code: ErrorCodeModelQuota, replaceToken: true},
		{name: "replaced token credential quota", status: 429, credentialScope: true, replaceToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			m, ids := durableTestManager(t, path)
			owner, _ := m.GetByID(ids[1])
			owner.Metadata = map[string]any{"access_token": "original-token", "refresh_token": "old-refresh"}
			if _, errUpdate := m.Update(context.Background(), owner); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			opts := durableTestOptions()
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[1]
			before, errPick := durableTestPick(m, "model-one", opts)
			if errPick != nil {
				t.Fatal(errPick)
			}
			updated := before.Clone()
			updated.Metadata["refresh_token"] = "new-refresh"
			if tc.replaceToken {
				updated.Metadata["access_token"] = "replacement-token"
			}
			current, errUpdate := m.Update(context.Background(), updated)
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if current.CredentialVersion <= before.CredentialVersion {
				t.Fatal("credential update did not advance the version")
			}
			cooldown := time.Hour
			m.MarkResult(context.Background(), Result{
				AuthID: before.ID, Provider: "claude", Model: "model-one",
				AccessToken: authAccessToken(before), CredentialVersion: before.CredentialVersion,
				RegistrationEpoch: before.RegistrationEpoch, CredentialScope: tc.credentialScope,
				Error: &Error{HTTPStatus: tc.status, Code: tc.code}, RetryAfter: &cooldown,
			})
			want := ids[1]
			if tc.migrate {
				want = ids[0]
			}
			picked, errPick := durableTestPick(m, "model-one", durableTestOptions())
			if tc.migrate && errPick != nil {
				t.Fatal(errPick)
			}
			if picked != nil && picked.ID != want {
				t.Fatalf("selected %s, want %s", picked.ID, want)
			}
			// Reopen the binding with healthy accounts to detect any unintended migration.
			restarted, _ := durableTestManager(t, path)
			picked, errPick = durableTestPick(restarted, "model-two", durableTestOptions())
			if errPick != nil || picked.ID != want {
				t.Fatalf("restored owner = %v, %v; want %s", picked, errPick, want)
			}
		})
	}
}

func TestDurableAffinityRefreshPreservesConfirmedModelQuota(t *testing.T) {
	withQuotaCooldownEnabled(t)
	m, _, primary, backup, model := newUnauthorizedRefreshFixture(t, false)
	path := filepath.Join(t.TempDir(), "bindings.json")
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}})
	t.Cleanup(selector.Stop)
	m.SetSelector(selector)
	opts := durableTestOptions()
	owner, _, _, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, model, opts, nil)
	if errPick != nil || owner.ID != primary.ID {
		t.Fatalf("initial owner = %v, %v", owner, errPick)
	}
	cooldown := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: primary.ID, Provider: "codex", Model: model,
		CredentialVersion: owner.CredentialVersion, RegistrationEpoch: owner.RegistrationEpoch,
		Error: &Error{Code: ErrorCodeModelQuota, HTTPStatus: 429}, RetryAfter: &cooldown})
	before, _ := m.GetByID(primary.ID)
	deadline := before.ModelStates[model].Quota.NextRecoverAt
	// A concurrent 401 must not let successful refresh erase confirmed quota.
	m.MarkResult(context.Background(), Result{AuthID: primary.ID, Provider: "codex", Model: model,
		CredentialVersion: owner.CredentialVersion, RegistrationEpoch: owner.RegistrationEpoch,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}})
	refreshed, errRefresh := m.ForceRefreshAuth(context.Background(), primary.ID)
	if errRefresh != nil {
		t.Fatal(errRefresh)
	}
	if refreshed.CredentialVersion <= owner.CredentialVersion {
		t.Fatal("refresh did not replace the credential")
	}
	if reason, reset := m.durableOwnerExhaustion(primary.ID, model); reason != ErrorCodeModelQuota || !reset.Equal(deadline) {
		t.Fatalf("refresh changed quota confirmation: %q, %v", reason, reset)
	}
	replacement, _, _, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, model, opts, nil)
	if errPick != nil || replacement.ID != backup.ID {
		t.Fatalf("confirmed quota no longer permits migration: %v, %v", replacement, errPick)
	}
	store := loadDurableSessions(path)
	if store.err != nil {
		t.Fatal(store.err)
	}
	sessionID, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if got := store.owner(durableSessionKey("codex", sessionID, opts)); got != backup.ID {
		t.Fatalf("persisted replacement = %q, want %q", got, backup.ID)
	}
}

func TestDurableAffinityCodexClientTokenPartitions(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, ua := range []string{"codex-tui/0.154.0", "curl/8"} {
		for _, tc := range []struct {
			name            string
			status          int
			code            string
			credentialScope bool
			replaceToken    bool
			migrate         bool
		}{
			{name: "401", status: 401},
			{name: "generic 429", status: 429},
			{name: "model quota", status: 429, code: ErrorCodeModelQuota, migrate: true},
			{name: "credential quota", status: 429, credentialScope: true, migrate: true},
			{name: "replaced token model quota", status: 429, code: ErrorCodeModelQuota, replaceToken: true},
			{name: "replaced token credential quota", status: 429, credentialScope: true, replaceToken: true},
		} {
			t.Run(ua+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "bindings.json")
				ids := []string{t.Name() + "-a", t.Name() + "-b"}
				newManager := func() *Manager {
					selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{File: path, Fallback: &FillFirstSelector{}})
					t.Cleanup(selector.Stop)
					m := NewManager(nil, selector, nil)
					m.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
					for _, id := range ids {
						_, errRegister := m.Register(WithSkipPersist(context.Background()), &Auth{
							ID: id, Provider: "codex", Status: StatusActive,
							Metadata: map[string]any{"access_token": "outer-token", "use_siwc": true,
								"siwc": map[string]any{"access_token": "siwc-token"}},
						})
						if errRegister != nil {
							t.Fatal(errRegister)
						}
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "model-one"}, {ID: "model-two"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					return m
				}
				m := newManager()
				opts := durableTestOptions()
				opts.Headers.Set("User-Agent", ua)
				opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = ids[1]
				before, _, _, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, "model-one", opts, nil)
				if errPick != nil {
					t.Fatal(errPick)
				}
				delete(opts.Metadata, cliproxyexecutor.PinnedAuthMetadataKey)
				updated := before.Clone()
				// Rotate the unused token, or both tokens for the stale-result cases.
				if ua == "codex-tui/0.154.0" || tc.replaceToken {
					updated.Metadata["siwc"].(map[string]any)["access_token"] = "new-siwc-token"
				}
				if ua == "curl/8" || tc.replaceToken {
					updated.Metadata["access_token"] = "new-outer-token"
				}
				current, errUpdate := m.Update(WithSkipPersist(context.Background()), updated)
				if errUpdate != nil || current.CredentialVersion <= before.CredentialVersion {
					t.Fatalf("credential rotation failed: %v", errUpdate)
				}
				cooldown := time.Hour
				m.recordExecutionResult(context.Background(), Result{
					AuthID: before.ID, Provider: "codex", Model: "model-one", Options: opts,
					Error: &Error{HTTPStatus: tc.status, Code: tc.code}, RetryAfter: &cooldown,
					CredentialScope: tc.credentialScope,
				}, before, false)
				want := ids[1]
				if tc.migrate {
					want = ids[0]
				}
				picked, _, _, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, "model-one", opts, nil)
				if tc.migrate || tc.replaceToken {
					if errPick != nil || picked == nil || picked.ID != want {
						t.Fatalf("selected = %v, %v; want %s", picked, errPick, want)
					}
				} else if errPick == nil || picked != nil {
					t.Fatalf("unconfirmed failure bypassed owner: %v, %v", picked, errPick)
				}
				restarted := newManager()
				picked, _, _, errPick = restarted.pickNextMixed(context.Background(), []string{"codex"}, "model-two", opts, nil)
				if errPick != nil || picked == nil || picked.ID != want {
					t.Fatalf("restored owner = %v, %v; want %s", picked, errPick, want)
				}
			})
		}
	}
}
