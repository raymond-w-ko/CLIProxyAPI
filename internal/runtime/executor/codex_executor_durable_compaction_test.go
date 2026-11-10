package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexDurableAffinityLocalCompactionQuotaPolicy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		mode := "execute"
		if stream {
			mode = "stream"
		}
		for _, failure := range []string{"none", "generic rate limit", "quota"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				const model = "durable-compact-model"
				var mu sync.Mutex
				var attempts []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Lock()
					attempts = append(attempts, key)
					mu.Unlock()
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/responses" || strings.Contains(string(body), "compaction_trigger") {
						t.Errorf("summary request was not prepared: path=%s body=%s", r.URL.Path, body)
					}
					if key == "second" && failure != "none" {
						w.Header().Set("Retry-After", "3600")
						w.WriteHeader(http.StatusTooManyRequests)
						if failure == "quota" {
							_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"Usage limit reached","resets_in_seconds":3600}}`)
						} else {
							_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"summary\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Saved summary\"}]}]}}\n\n")
				}))
				t.Cleanup(server.Close)
				cfg := &config.Config{}
				for _, key := range []string{"first", "second"} {
					cfg.CodexKey = append(cfg.CodexKey, config.CodexKey{APIKey: key, BaseURL: server.URL, Models: []config.CodexModel{{Name: model, IsCompat: true}}})
				}
				path := filepath.Join(t.TempDir(), "bindings.json")
				ids := []string{t.Name() + "-a", t.Name() + "-b"}
				newManager := func() *cliproxyauth.Manager {
					selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{File: path, Fallback: &cliproxyauth.FillFirstSelector{}})
					t.Cleanup(selector.Stop)
					manager := cliproxyauth.NewManager(nil, selector, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(2, 0, 0)
					manager.RegisterExecutor(NewCodexExecutor(cfg))
					for i, key := range []string{"first", "second"} {
						id := ids[i]
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model, IsCompat: true}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						_, errRegister := manager.Register(cliproxyauth.WithSkipPersist(t.Context()), &cliproxyauth.Auth{ID: id, Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"api_key": key, "base_url": server.URL}})
						if errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					return manager
				}
				manager := newManager()
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"X-Session-Id": {"stable-compaction"}}, Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: ids[1]}}
				if _, errPick := manager.SelectAuth(t.Context(), "codex", model, opts); errPick != nil {
					t.Fatal(errPick)
				}
				delete(opts.Metadata, cliproxyexecutor.PinnedAuthMetadataKey)
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"durable-compact-model","input":[{"type":"message","role":"user","content":"remember this"},{"type":"compaction_trigger"}]}`)}
				var err error
				if stream {
					var completed bool
					var result *cliproxyexecutor.StreamResult
					result, err = manager.ExecuteStream(t.Context(), []string{"codex"}, req, opts)
					if result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							completed = completed || strings.Contains(string(chunk.Payload), `"type":"response.completed"`)
						}
					}
					if err == nil && !completed {
						t.Fatal("compaction stream ended without completion")
					}
				} else {
					var result cliproxyexecutor.Response
					result, err = manager.Execute(t.Context(), []string{"codex"}, req, opts)
					if err == nil && ojson.GetBytesNoCopy(result.Payload, "object").String() != "response.compaction" {
						t.Fatalf("expected local compaction, got %s", result.Payload)
					}
				}
				if (err != nil) != (failure == "generic rate limit") {
					t.Fatalf("compaction error = %v for %s", err, failure)
				}
				wantAttempts, wantOwner := "second", ids[1]
				if failure == "quota" {
					wantAttempts, wantOwner = "second,first", ids[0]
				}
				mu.Lock()
				gotAttempts := strings.Join(attempts, ",")
				mu.Unlock()
				if gotAttempts != wantAttempts {
					t.Fatalf("attempts=%s, want %s", gotAttempts, wantAttempts)
				}
				restarted := newManager()
				owner, errPick := restarted.SelectAuth(t.Context(), "codex", model, opts)
				if errPick != nil || owner == nil || owner.ID != wantOwner {
					t.Fatalf("saved owner=%v, %v; want %s", owner, errPick, wantOwner)
				}
			})
		}
	}
}
