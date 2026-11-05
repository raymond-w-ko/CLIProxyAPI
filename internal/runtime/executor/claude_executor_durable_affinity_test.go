package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type durableClaudeTestExecutor struct{ *ClaudeExecutor }

func (e durableClaudeTestExecutor) ForAPIKey() cliproxyauth.ProviderExecutor { return e }

// Exercise the native count endpoint against the test server; custom production
// origins normally use local estimation instead.
func (e durableClaudeTestExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.countTokensUpstream(ctx, auth, req, opts)
}

func TestClaudeDurableAffinityAndDateSurviveRestart(t *testing.T) {
	homekv.ClearCurrent()
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatal(errShutdown)
	}
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(func() {
		claudeCodeCurrentTimeFunc = nil
		homekv.ClearCurrent()
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
		helps.ResetClaudeDiagnosticsForTest()
	})
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kv.db")
	bindingPath := filepath.Join(dir, "session-bindings.json")
	clock := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	claudeCodeCurrentTimeFunc = func(*config.Config, *cliproxyauth.Auth) time.Time { return clock }
	zero := 0
	cfg := &config.Config{Claude: config.ClaudeConfig{SessionDatePinDays: &zero}}
	var activeSelector *cliproxyauth.SessionAffinitySelector
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}, {ID: "claude-sonnet-4-6"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	start := func(preferred int) *cliproxyauth.Manager {
		t.Helper()
		if errOpen := kvstore.ConfigureLocal(true, dbPath); errOpen != nil {
			t.Fatal(errOpen)
		}
		selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
			File: bindingPath, Fallback: &cliproxyauth.FillFirstSelector{},
		})
		activeSelector = selector
		t.Cleanup(selector.Stop)
		manager := cliproxyauth.NewManager(nil, selector, nil)
		manager.SetRetryConfig(3, 0, 0)
		manager.RegisterExecutor(NewClaudeExecutor(cfg))
		for i, id := range ids {
			priority := "0"
			if i == preferred {
				priority = "10"
			}
			_, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
				ID: id, Provider: "claude",
				Attributes: map[string]string{
					"api_key": "sk-ant-oat-restart-" + strconv.Itoa(i), "priority": priority,
					"cloak_mode": "always", "timezone": "UTC",
				},
				Metadata: map[string]any{"account_uuid": "11111111-2222-4333-8444-55555555555" + strconv.Itoa(i)},
			})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
		}
		return manager
	}
	restart := func(preferred int) *cliproxyauth.Manager {
		t.Helper()
		activeSelector.Stop()
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Fatal(errShutdown)
		}
		helps.ResetClaudeDiagnosticsForTest()
		return start(preferred)
	}
	var bodies [][]byte
	var keys []string
	quota := false
	transport := datePinRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		bodies = append(bodies, body)
		key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		keys = append(keys, key)
		header := http.Header{"Content-Type": {"application/json"}}
		status := http.StatusOK
		payload := `{"id":"msg_restart","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if quota && key == "sk-ant-oat-restart-0" {
			status = http.StatusTooManyRequests
			header.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			header.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			payload = `{"type":"error","error":{"type":"rate_limit_error","message":"Quota exhausted"}}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Session-Id": {"durable-date-restart"}}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"Say OK."}],"max_tokens":16}`)}
	invoke := func(manager *cliproxyauth.Manager) {
		t.Helper()
		if _, errExecute := manager.Execute(ctx, []string{"claude"}, req, opts); errExecute != nil {
			t.Fatal(errExecute)
		}
	}
	invoke(start(0))
	clock = clock.AddDate(0, 0, 45)
	// A fresh selector would prefer B. The saved owner must retain A across
	// restart and a model change, while the saved date must retain day one.
	manager := restart(1)
	req.Model = "claude-sonnet-4-6"
	req.Payload = []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Say OK."}],"max_tokens":16}`)
	invoke(manager)
	quota = true
	invoke(manager)
	quota = false
	// Both credentials are healthy after this restart, and A has higher
	// priority. Only the saved replacement binding can retain B.
	manager = restart(0)
	invoke(manager)
	wantKeys := []string{"sk-ant-oat-restart-0", "sk-ant-oat-restart-0", "sk-ant-oat-restart-0", "sk-ant-oat-restart-1", "sk-ant-oat-restart-1"}
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("upstream owners = %v, want %v", keys, wantKeys)
	}
	for i, body := range bodies {
		if got := extractClaudeCloakDate(t, body); got != "2026-08-01" {
			t.Fatalf("request %d date = %q, want 2026-08-01", i, got)
		}
	}
	// A new session must get today's date, not another session's saved pin.
	opts.Headers.Set("X-Session-Id", "new-session-after-restart")
	invoke(manager)
	if got := extractClaudeCloakDate(t, bodies[len(bodies)-1]); got != clock.Format("2006-01-02") {
		t.Fatalf("new session date = %q, want %s", got, clock.Format("2006-01-02"))
	}
}

func TestClaudeDurableAffinityModelQuota(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count", "compact", "compact-stream"} {
		for _, scenarioName := range []string{"model quota", "both exhausted", "generic 429", "per-minute rate", "overage billing", "model quota with billing", "model quota without 5h", "overage billing without 5h"} {
			t.Run(mode+"/"+scenarioName, func(t *testing.T) {
				scenario := strings.TrimSuffix(scenarioName, " without 5h")
				missing5h := scenario != scenarioName
				const model = "claude-fable-5-1"
				var mu sync.Mutex
				attempts := map[string]int{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					key := r.Header.Get("X-Api-Key")
					if key == "" {
						key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					}
					mu.Lock()
					attempts[key]++
					mu.Unlock()
					if key == "first" || scenario == "both exhausted" {
						if scenario == "generic 429" {
							w.Header().Set("Retry-After", "3600")
						} else if scenario == "per-minute rate" {
							w.Header().Set("Retry-After", "30")
							w.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(4*time.Hour).Unix(), 10))
						} else if scenario != "overage billing" {
							w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
							if !missing5h {
								w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
							}
							w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
							w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
							w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
						}
						if scenario == "overage billing" || scenario == "model quota with billing" {
							billingReset := strconv.FormatInt(time.Now().Add(31*24*time.Hour).Unix(), 10)
							w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
							w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "overage")
							if missing5h {
								w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
							}
							w.Header().Set("Anthropic-Ratelimit-Unified-Reset", billingReset)
							w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Reset", billingReset)
						}
						w.WriteHeader(http.StatusTooManyRequests)
						if scenario == "per-minute rate" {
							_, _ = w.Write(claudePerMinuteBody("requests"))
							return
						}
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`)
						return
					}
					if mode == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-fable-5-1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else if mode == "count" {
						_, _ = io.WriteString(w, `{"input_tokens":1}`)
					} else {
						_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","model":"claude-fable-5-1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
					}
				}))
				defer server.Close()
				bindingPath := filepath.Join(t.TempDir(), "bindings.json")
				selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
					File: bindingPath, Fallback: &cliproxyauth.FillFirstSelector{},
				})
				defer selector.Stop()
				manager := cliproxyauth.NewManager(nil, selector, nil)
				manager.SetRetryConfig(3, 30*time.Second, 0)
				if scenario == "overage billing" || scenario == "per-minute rate" {
					// Exercise one rejection without waiting through generic backoff.
					manager.SetRetryConfig(0, 0, 0)
				}
				manager.RegisterExecutor(durableClaudeTestExecutor{NewClaudeExecutor(&config.Config{})})
				ids := []string{t.Name() + "-a", t.Name() + "-b"}
				for i, key := range []string{"first", "second"} {
					id := ids[i]
					registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}, {ID: "claude-opus-5"}})
					defer registry.GetGlobalRegistry().UnregisterClient(id)
					_, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
						ID: id, Provider: "claude", Attributes: map[string]string{"api_key": key, "base_url": server.URL},
					})
					if errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Session-Id": {"stable"}}}
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"test"}],"max_tokens":16}`)}
				if mode == "compact" || mode == "compact-stream" {
					opts.SourceFormat = sdktranslator.FormatOpenAIResponse
					opts.Alt = "responses/compact"
					req.Payload = []byte(`{"model":"claude-fable-5-1","input":"Summarize this test conversation."}`)
				}
				invoke := func() error {
					switch mode {
					case "stream", "compact-stream":
						stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
						if errStream != nil {
							return errStream
						}
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								return chunk.Err
							}
						}
						return nil
					case "count":
						_, errCount := manager.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
						return errCount
					default:
						_, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts)
						return errExecute
					}
				}
				errExecute := invoke()
				if scenario == "model quota" || scenario == "model quota with billing" {
					if errExecute != nil {
						t.Fatal(errExecute)
					}
				} else {
					var status interface{ StatusCode() int }
					if !errors.As(errExecute, &status) || status.StatusCode() != 429 {
						t.Fatalf("expected terminal 429, got %v", errExecute)
					}
					// Repeated client requests during cooldown must not send more upstream traffic.
					if scenario != "overage billing" {
						if errAgain := invoke(); errAgain == nil {
							t.Fatal("expected cooldown rejection")
						}
					}
				}
				mu.Lock()
				first, second := attempts["first"], attempts["second"]
				mu.Unlock()
				wantSecond := 1
				if scenario == "generic 429" || scenario == "per-minute rate" || scenario == "overage billing" {
					wantSecond = 0
				}
				if first != 1 || second != wantSecond {
					t.Fatalf("upstream attempts: first=%d second=%d; want 1 and %d", first, second, wantSecond)
				}
				if scenario == "per-minute rate" {
					failed, _ := manager.GetByID(ids[0])
					state := failed.ModelStates[model]
					if state == nil || !state.NextRetryAfter.After(time.Now()) || state.NextRetryAfter.After(time.Now().Add(time.Minute)) {
						t.Fatalf("per-minute rate limit inherited the long quota reset: %+v", state)
					}
					if state.Quota.Reason == cliproxyauth.ErrorCodeModelQuota || failed.Quota.Reason == "credential_quota" {
						t.Fatal("per-minute rate limit became confirmed quota exhaustion")
					}
				}
				if scenario == "overage billing" || scenario == "model quota with billing" {
					failed, _ := manager.GetByID(ids[0])
					state := failed.ModelStates[model]
					if state == nil || state.NextRetryAfter.After(time.Now().Add(2*time.Hour)) {
						t.Fatalf("billing reset became a long cooldown: %+v", state)
					}
				}
				if missing5h {
					failed, _ := manager.GetByID(ids[0])
					state := failed.ModelStates[model]
					if failed.Quota.Reason == "credential_quota" || state == nil {
						t.Fatalf("missing 5h headers caused account-wide exhaustion: %+v", failed)
					}
					if got := state.Quota.Reason == cliproxyauth.ErrorCodeModelQuota; got != (scenario == "model quota") {
						t.Fatalf("model quota classification = %q for %s", state.Quota.Reason, scenario)
					}
				}
				// No account-wide cooldown: a healthy sibling model keeps the current owner.
				owner, errSelect := manager.SelectAuth(context.Background(), "claude", "claude-opus-5", opts)
				if errSelect != nil || owner.ID != ids[wantSecond] {
					t.Fatalf("whole-session owner = %v, %v", owner, errSelect)
				}
				// Reload only durable ownership, with healthy credentials. A lost
				// binding would select the first account after a quota migration.
				selector.Stop()
				reloaded := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
					File: bindingPath, Fallback: &cliproxyauth.FillFirstSelector{},
				})
				defer reloaded.Stop()
				restarted := cliproxyauth.NewManager(nil, reloaded, nil)
				restarted.RegisterExecutor(durableClaudeTestExecutor{NewClaudeExecutor(&config.Config{})})
				for i, id := range ids {
					_, errRegister := restarted.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
						ID: id, Provider: "claude", Attributes: map[string]string{"api_key": []string{"first", "second"}[i], "base_url": server.URL},
					})
					if errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				owner, errSelect = restarted.SelectAuth(context.Background(), "claude", "claude-opus-5", opts)
				if errSelect != nil || owner.ID != ids[wantSecond] {
					t.Fatalf("restarted whole-session owner = %v, %v", owner, errSelect)
				}
			})
		}
	}
}

func TestClaudeDurableAffinityTruncatedStreamKeepsOwner(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		t.Run(format.String(), func(t *testing.T) {
			const model = "claude-sonnet-4-6"
			var mu sync.Mutex
			attempts := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-Api-Key")
				if key == "" {
					key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				mu.Lock()
				attempts[key]++
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_truncated\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n")
				w.(http.Flusher).Flush()
				// EOF without message_stop must be an error, not a successful response.
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "bindings.json")
			selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
				File: path, Fallback: &cliproxyauth.FillFirstSelector{},
			})
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
			ids := []string{t.Name() + "-a", t.Name() + "-b"}
			for i, key := range []string{"first", "second"} {
				registry.GetGlobalRegistry().RegisterClient(ids[i], "claude", []*registry.ModelInfo{{ID: model}, {ID: "claude-opus-5"}})
				defer registry.GetGlobalRegistry().UnregisterClient(ids[i])
				if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
					ID: ids[i], Provider: "claude", Attributes: map[string]string{"api_key": key, "base_url": server.URL},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			opts := cliproxyexecutor.Options{SourceFormat: format, Headers: http.Header{"X-Session-Id": {"stable"}}}
			payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"max_tokens":16}`)
			if format == sdktranslator.FormatOpenAIResponse {
				payload = []byte(`{"model":"claude-sonnet-4-6","input":"test"}`)
			}
			stream, streamErr := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: payload}, opts)
			if streamErr == nil {
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
			}
			var status interface{ StatusCode() int }
			if !errors.As(streamErr, &status) || status.StatusCode() != http.StatusBadGateway {
				t.Fatalf("truncated stream error = %v, want HTTP 502", streamErr)
			}
			mu.Lock()
			first, second := attempts["first"], attempts["second"]
			mu.Unlock()
			if first != 1 || second != 0 {
				t.Fatalf("truncated stream changed accounts: first=%d second=%d", first, second)
			}
			// Reload durable ownership into a fresh selector, with the same credentials.
			reloaded := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
				File: path, Fallback: &cliproxyauth.FillFirstSelector{},
			})
			defer reloaded.Stop()
			manager.SetSelector(reloaded)
			owner, errSelect := manager.SelectAuth(context.Background(), "claude", "claude-opus-5", opts)
			if errSelect != nil || owner.ID != ids[0] {
				t.Fatalf("truncation changed persisted ownership: %v, %v", owner, errSelect)
			}
		})
	}
}

func TestClaudeModelQuotaRequiresExplicitRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{"generic", nil, false},
		{"retry hint only", http.Header{"Retry-After": {"3600"}}, false},
		{"healthy model", http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Status": {"allowed"}}, false},
		{"disabled overage only", http.Header{"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"}}, false},
		{"rejected model", http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyClaudeUpstreamError(429, tc.headers, []byte(`{"error":{"type":"rate_limit_error"}}`))
			var quota interface{ IsModelQuotaExhausted() bool }
			got := errors.As(err, &quota) && quota.IsModelQuotaExhausted()
			if got != tc.want {
				t.Fatalf("confirmed model quota=%t, want %t", got, tc.want)
			}
			fast := wrapClaudeFastRequestError(true, 429, err)
			if scoped, ok := fast.(interface{ IsRequestScoped() bool }); !ok || !scoped.IsRequestScoped() {
				t.Fatal("fast-mode errors must retain request-scoped handling")
			}
		})
	}
}

func TestClaudeDurableAffinityApplyPatchFailureKeepsOwner(t *testing.T) {
	for _, mode := range []string{"nonstream", "stream"} {
		t.Run(mode, func(t *testing.T) {
			const model = "claude-sonnet-4-6"
			var mu sync.Mutex
			attempts := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-Api-Key")
				if key == "" {
					key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
				mu.Lock()
				attempts[key]++
				mu.Unlock()
				// Claude uses an upstream stream for Responses translation in both modes.
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, task6ProviderFixture("claude", "stream", "apply_patch"))
			}))
			defer server.Close()
			selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
				File: filepath.Join(t.TempDir(), "bindings.json"), Fallback: &cliproxyauth.FillFirstSelector{},
			})
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
			ids := []string{t.Name() + "-a", t.Name() + "-b"}
			for i, key := range []string{"first", "second"} {
				id := ids[i]
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}, {ID: "claude-opus-5"}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
				if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
					ID: id, Provider: "claude", Attributes: map[string]string{"api_key": key, "base_url": server.URL},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"X-Session-Id": {"stable"}}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(task6PatchRequest)}
			if mode == "stream" {
				stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				if errStream != nil {
					assertTask6PatchError(t, errStream)
				} else {
					assertTask6FailedStream(t, stream.Chunks)
				}
			} else {
				_, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts)
				assertTask6PatchError(t, errExecute)
			}
			mu.Lock()
			first, second := attempts["first"], attempts["second"]
			mu.Unlock()
			if first != 1 || second != 0 {
				t.Fatalf("patch failure changed accounts: first=%d second=%d", first, second)
			}
			owner, errSelect := manager.SelectAuth(context.Background(), "claude", "claude-opus-5", opts)
			if errSelect != nil || owner.ID != ids[0] {
				t.Fatalf("patch failure changed durable ownership: %v, %v", owner, errSelect)
			}
		})
	}
}

func TestClaudeDurableAffinityUnsupportedAttachmentKeepsOwner(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			const model = "claude-sonnet-4-6"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("unsupported attachment reached upstream")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "bindings.json")
			selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
				File: path, Fallback: &cliproxyauth.FillFirstSelector{},
			})
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetRetryConfig(3, 30*time.Second, 0)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
			ids := []string{t.Name() + "-a", t.Name() + "-b"}
			for _, id := range ids {
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
				if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
					ID: id, Provider: "claude", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: http.Header{"X-Session-Id": {"stable"}}}
			// Pin the second credential so a lost binding cannot pass via fill-first.
			pinned := opts
			pinned.Metadata = map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: ids[1]}
			if owner, errSelect := manager.SelectAuth(context.Background(), "claude", model, pinned); errSelect != nil || owner.ID != ids[1] {
				t.Fatalf("initial owner = %v, %v", owner, errSelect)
			}
			before, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-not-stored"}}]}]}`)}
			var errExecute error
			switch mode {
			case "execute":
				_, errExecute = manager.Execute(context.Background(), []string{"claude"}, req, opts)
			case "stream":
				_, errExecute = manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
			case "count":
				_, errExecute = manager.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
			}
			var status interface{ StatusCode() int }
			if !errors.As(errExecute, &status) || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("unsupported attachment returned %v, want 400", errExecute)
			}
			if owner, errSelect := manager.SelectAuth(context.Background(), "claude", model, opts); errSelect != nil || owner.ID != ids[1] {
				t.Fatalf("translation failure cooled or changed owner: %v, %v", owner, errSelect)
			}
			after, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if string(before) != string(after) {
				t.Fatal("translation failure changed persisted bindings")
			}
		})
	}
}
