package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexWebsocketEarlyRejectionPreservesDurableQuotaPolicy(t *testing.T) {
	for _, buffering := range []bool{false, true} {
		for _, tc := range []struct {
			name, frame string
			migrate     bool
		}{
			{"capacity", `{"type":"error","status":429,"headers":{"Retry-After":"3600"},"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded","message":"Codex is at capacity"}}`, false},
			{"generic rate limit", `{"type":"error","status":429,"headers":{"Retry-After":"3600"},"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`, false},
			{"quota error", `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"Usage limit reached","resets_in_seconds":3600}}`, true},
			{"quota terminal", `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","status_code":429,"message":"Usage limit reached","resets_in_seconds":3600}}}`, true},
		} {
			t.Run(fmt.Sprintf("%s/buffering=%t", tc.name, buffering), func(t *testing.T) {
				var mu sync.Mutex
				var attempts []string
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, errUpgrade := upgrader.Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Errorf("upgrade: %v", errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						t.Errorf("read create: %v", errRead)
						return
					}
					key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Lock()
					attempts = append(attempts, key)
					mu.Unlock()
					frame := codexCompletedEventBody
					if key == "first" {
						frame = tc.frame
					}
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
						t.Errorf("write reply: %v", errWrite)
					}
					// Let the executor invalidate rejected sockets so a server EOF cannot mask the rejection.
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
					}
				}))
				t.Cleanup(server.Close)
				cfg := codexBufferingConfig(buffering)
				exec := NewCodexWebsocketsExecutor(cfg)
				exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				const sessionID = "durable-websocket"
				t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
				disconnect := exec.UpstreamDisconnectChan(sessionID)
				bindingPath := filepath.Join(t.TempDir(), "bindings.json")
				ids := []string{t.Name() + "-a", t.Name() + "-b"}
				newManager := func() *cliproxyauth.Manager {
					selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
						File: bindingPath, Fallback: &cliproxyauth.FillFirstSelector{},
					})
					t.Cleanup(selector.Stop)
					manager := cliproxyauth.NewManager(nil, selector, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(2, 0, 0)
					manager.RegisterExecutor(exec)
					for i, key := range []string{"first", "second"} {
						id := ids[i]
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.6-terra"}, {ID: "gpt-6-astra"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						if _, errRegister := manager.Register(cliproxyauth.WithSkipPersist(context.Background()), &cliproxyauth.Auth{
							ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
							Attributes: map[string]string{"api_key": key, "base_url": server.URL},
						}); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					return manager
				}
				manager := newManager()
				req, opts := codexWebsocketRequest()
				opts.Headers = http.Header{"X-Session-Id": {sessionID}}
				opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}
				stream, errStream := manager.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), []string{"codex"}, req, opts)
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							errStream = chunk.Err
						}
					}
				}
				mu.Lock()
				gotAttempts := strings.Join(attempts, ",")
				mu.Unlock()
				wantAttempts, wantOwner := "first", ids[0]
				if tc.migrate {
					wantAttempts, wantOwner = "first,second", ids[1]
				}
				if gotAttempts != wantAttempts || (errStream == nil) != tc.migrate {
					t.Fatalf("attempts=%s error=%v; want attempts=%s migration=%t", gotAttempts, errStream, wantAttempts, tc.migrate)
				}
				select {
				case errDisconnect := <-disconnect:
					t.Fatalf("early rejection closed downstream: %v", errDisconnect)
				default:
				}
				// Reload healthy accounts: neither a restart nor another model may reclaim a migrated session.
				restarted := newManager()
				for _, model := range []string{"gpt-5.6-terra", "gpt-6-astra"} {
					owner, errPick := restarted.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{Headers: opts.Headers})
					if errPick != nil || owner == nil || owner.ID != wantOwner {
						t.Fatalf("restored owner on %s = %v, %v; want %s", model, owner, errPick, wantOwner)
					}
				}
			})
		}
	}
}
