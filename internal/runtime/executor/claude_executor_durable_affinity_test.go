package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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

func TestClaudeDurableAffinityModelQuota(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, scenario := range []string{"model quota", "both exhausted", "generic 429"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
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
						} else {
							w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
							w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
							w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
							w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
							w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
						}
						w.WriteHeader(http.StatusTooManyRequests)
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
				selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
					File: filepath.Join(t.TempDir(), "bindings.json"), Fallback: &cliproxyauth.FillFirstSelector{},
				})
				defer selector.Stop()
				manager := cliproxyauth.NewManager(nil, selector, nil)
				manager.SetRetryConfig(3, 30*time.Second, 0)
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
				invoke := func() error {
					switch mode {
					case "stream":
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
				if scenario == "model quota" {
					if errExecute != nil {
						t.Fatal(errExecute)
					}
				} else {
					var status interface{ StatusCode() int }
					if !errors.As(errExecute, &status) || status.StatusCode() != 429 {
						t.Fatalf("expected terminal 429, got %v", errExecute)
					}
					// Repeated client requests during cooldown must not send more upstream traffic.
					if errAgain := invoke(); errAgain == nil {
						t.Fatal("expected cooldown rejection")
					}
				}
				mu.Lock()
				first, second := attempts["first"], attempts["second"]
				mu.Unlock()
				wantSecond := 1
				if scenario == "generic 429" {
					wantSecond = 0
				}
				if first != 1 || second != wantSecond {
					t.Fatalf("upstream attempts: first=%d second=%d; want 1 and %d", first, second, wantSecond)
				}
				// No account-wide cooldown: a healthy sibling model keeps the current owner.
				owner, errSelect := manager.SelectAuth(context.Background(), "claude", "claude-opus-5", opts)
				if errSelect != nil || owner.ID != ids[wantSecond] {
					t.Fatalf("whole-session owner = %v, %v", owner, errSelect)
				}
			})
		}
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
