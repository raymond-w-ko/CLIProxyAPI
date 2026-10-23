package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestStreamResultRetainsRequestTokenAfterGinReuse(t *testing.T) {
	for _, tc := range []struct {
		name, initialUA, reusedUA, wantToken string
	}{
		{"outer", "codex-tui/0.154.0", "curl/8", "outer-token"},
		{"siwc", "curl/8", "codex-tui/0.154.0", "siwc-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			auth, errRegister := m.Register(WithSkipPersist(context.Background()), &Auth{
				ID: "stream-token", Provider: "codex", Status: StatusActive,
				Metadata: map[string]any{
					"access_token": "outer-token", "use_siwc": true,
					"siwc": map[string]any{"access_token": "siwc-token"},
				},
			})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			request.Header.Set("User-Agent", tc.initialUA)
			ginCtx := &gin.Context{Request: request}
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			remaining := make(chan cliproxyexecutor.StreamChunk)
			result := m.wrapStreamResult(ctx, auth, "codex", "model", "model", nil, nil, remaining, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{}, 0)
			// The handler has returned; Gin can now belong to another request.
			reused := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			reused.Header.Set("User-Agent", tc.reusedUA)
			ginCtx.Request = reused
			remaining <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}}
			close(remaining)
			for range result.Chunks {
			}
			current, _ := m.GetByID(auth.ID)
			if current.RejectedAccessToken != tc.wantToken {
				t.Fatalf("rejected token = %q, want original request token %q", current.RejectedAccessToken, tc.wantToken)
			}
		})
	}
}
