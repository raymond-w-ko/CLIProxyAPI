package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestDurableAffinityThreadReplayRequiresConfirmedQuota(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, operation := range []string{"execute", "stream"} {
		for _, tc := range []struct {
			name, rebind string
			failure      Error
			migrate      bool
		}{
			{"generic rate limit", "quota", Error{HTTPStatus: 429, Message: "rate limited"}, false},
			{"authentication", "quota", Error{HTTPStatus: 401, Message: "unauthorized"}, false},
			{"quota", "quota", Error{HTTPStatus: 429, Code: ErrorCodeModelQuota}, true},
			{"never", "never", Error{HTTPStatus: 429, Code: ErrorCodeModelQuota}, false},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "bindings.json")
				manager, ids := durableTestManagerWithRebindOn(t, path, tc.rebind)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(&authFallbackExecutor{id: "claude"})
				opts := durableTestOptions()
				opts.SourceFormat = sdktranslator.FormatClaude
				opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = t.Name()
				execute := func(payload string) error {
					req := cliproxyexecutor.Request{Model: "model-one", Payload: []byte(payload)}
					if operation == "execute" {
						_, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts)
						return errExecute
					}
					response, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
					if response != nil {
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								errStream = chunk.Err
							}
						}
					}
					return errStream
				}
				const create = `{"thread":{"type":"create"},"messages":[{"role":"user","content":"full conversation"}]}`
				if errExecute := execute(create); errExecute != nil {
					t.Fatal(errExecute)
				}
				cliproxysession.RememberClaudeThreadAuth(t.Name(), "durable-message", ids[0])
				cooldown := time.Hour
				manager.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Error: &tc.failure, RetryAfter: &cooldown})
				before, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				errContinue := execute(`{"thread":{"type":"continue","previous_message_id":"durable-message"},"messages":[{"role":"user","content":"next"}]}`)
				if statusCodeFromError(errContinue) != http.StatusNotFound || errorCodeFromError(errContinue) != "thread_not_found" {
					t.Fatalf("continuation error=%v; want 404 thread_not_found", errContinue)
				}
				after, errRead := os.ReadFile(path)
				if errRead != nil || string(after) != string(before) {
					t.Fatalf("Thread replay signal changed ownership: %v", errRead)
				}
				if errReplay := execute(create); (errReplay == nil) != tc.migrate {
					t.Fatalf("full replay error=%v; permitted migration=%t", errReplay, tc.migrate)
				}
				restarted, _ := durableTestManagerWithRebindOn(t, path, tc.rebind)
				wantOwner := ids[0]
				if tc.migrate {
					wantOwner = ids[1]
				}
				owner, errPick := durableTestPick(restarted, "model-two", opts)
				if errPick != nil || owner == nil || owner.ID != wantOwner {
					t.Fatalf("restored owner=%v, error=%v; want %s", owner, errPick, wantOwner)
				}
			})
		}
	}
}
