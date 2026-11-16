package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Exercise the handler's in-place unpin/replay loop, not only Manager retries.
func TestResponsesWebsocketDurablePinnedFailoverRequiresQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, buffering := range []bool{false, true} {
		for _, tc := range []struct {
			name, frame, rebind string
			migrate             bool
		}{
			{"quota", `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"Usage limit reached","resets_in_seconds":3600}}`, "quota", true},
			{"rate limit", `{"type":"error","status":429,"headers":{"Retry-After":"3600"},"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`, "quota", false},
			{"never", `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"Usage limit reached","resets_in_seconds":3600}}`, "never", false},
		} {
			t.Run(fmt.Sprintf("%s/buffering=%t", tc.name, buffering), func(t *testing.T) {
				var reject atomic.Bool
				var callsA, callsB atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Error(errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
						key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
						if key == "a" {
							callsA.Add(1)
							if reject.Load() {
								if buffering {
									_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"failed-a","status":"in_progress","output":[]}}`))
								}
								_ = conn.WriteMessage(websocket.TextMessage, []byte(tc.frame))
								continue
							}
						} else if key == "b" {
							callsB.Add(1)
						} else {
							t.Errorf("unexpected test credential %q", key)
							return
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%s","status":"completed","output":[]}}`, key)))
					}
				}))
				t.Cleanup(upstream.Close)
				cfg := &config.Config{RequestRetry: 2, Codex: config.CodexConfig{StreamBootstrapBuffering: buffering}}
				model := "durable-pinned-" + strings.ReplaceAll(t.Name(), "/", "-")
				path := filepath.Join(t.TempDir(), "bindings.json")
				newManager := func() *coreauth.Manager {
					selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{File: path, Fallback: &coreauth.FillFirstSelector{}, RebindOn: tc.rebind})
					t.Cleanup(selector.Stop)
					manager := coreauth.NewManager(nil, selector, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(2, 0, 0)
					manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
					for i, key := range []string{"a", "b"} {
						id := model + "-" + key
						if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
							ID: id, Provider: "codex", Status: coreauth.StatusActive,
							Attributes: map[string]string{"api_key": key, "base_url": upstream.URL, "websockets": "true", "priority": fmt.Sprint(2 - i)},
						}); errRegister != nil {
							t.Fatal(errRegister)
						}
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					return manager
				}
				manager := newManager()
				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
				router := gin.New()
				router.GET("/v1/responses", h.ResponsesWebsocket)
				downstream := httptest.NewServer(router)
				t.Cleanup(downstream.Close)
				headers := http.Header{"X-Session-Id": {"durable-handler-session"}}
				conn, response, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", headers)
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				if errDial != nil {
					t.Fatal(errDial)
				}
				defer func() { _ = conn.Close() }()
				turn := func() []byte {
					t.Helper()
					payload := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":"hello"}]}`, model)
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(payload)); errWrite != nil {
						t.Fatal(errWrite)
					}
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					for {
						_, frame, errRead := conn.ReadMessage()
						if errRead != nil {
							t.Fatal(errRead)
						}
						if typ := ojson.GetBytesNoCopy(frame, "type").String(); typ == "error" || typ == "response.completed" {
							return frame
						}
					}
				}
				if frame := turn(); ojson.GetBytesNoCopy(frame, "response.id").String() != "resp-a" {
					t.Fatalf("first turn failed: %s", frame)
				}
				reject.Store(true)
				frame := turn()
				wantOwner := model + "-a"
				if tc.migrate {
					wantOwner = model + "-b"
					if ojson.GetBytesNoCopy(frame, "response.id").String() != "resp-b" || callsB.Load() != 1 {
						t.Fatalf("quota did not recover on B: %s; B calls=%d", frame, callsB.Load())
					}
				} else if ojson.GetBytesNoCopy(frame, "type").String() != "error" || callsB.Load() != 0 {
					t.Fatalf("owner escaped without permitted migration: %s; B calls=%d", frame, callsB.Load())
				}
				if callsA.Load() != 2 {
					t.Fatalf("A calls=%d, want 2", callsA.Load())
				}
				// Healthy state after restart must still load the final durable owner.
				restarted := newManager()
				owner, errPick := restarted.SelectAuth(context.Background(), "codex", model, coreexecutor.Options{Headers: headers})
				if errPick != nil || owner == nil || owner.ID != wantOwner {
					t.Fatalf("restored owner=%v, error=%v, want %s", owner, errPick, wantOwner)
				}
			})
		}
	}
}
