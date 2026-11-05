package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeQuotaFetchTransport(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	a := &cliproxyauth.Auth{ID: "quota", Provider: "claude", Metadata: map[string]any{"access_token": "sk-ant-oat01-quota-test"}}
	for _, status := range []int{200, 401, 403, 429, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "https://api.anthropic.com/api/oauth/usage" || req.Header.Get("Authorization") != "Bearer sk-ant-oat01-quota-test" || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
					t.Fatal("incorrect quota request")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"120"}, "Location": {"https://other.invalid/"}}, Body: io.NopCloser(strings.NewReader(`{"five_hour":null,"seven_day":{"utilization":28,"resets_at":"2030-01-01T00:00:00Z"}}`))}, nil
			}))
			windows, errFetch := e.FetchClaudeQuota(ctx, a)
			if status == 200 {
				if errFetch != nil || windows["7d"].Used != .28 || !windows["5h"].Unlimited {
					t.Fatalf("quota = %v, %v", windows, errFetch)
				}
			} else {
				var probe *cliproxyauth.ClaudeQuotaProbeError
				if !errors.As(errFetch, &probe) || probe.Status != status || probe.RetryAt.Before(time.Now().Add(time.Minute)) {
					t.Fatalf("error = %v", errFetch)
				}
			}
			if calls != 1 {
				t.Fatal("followed redirect or retried")
			}
		})
	}
}
func TestClaudeQuotaFetchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	a := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "sk-ant-oat01-test"}}
	if _, errFetch := NewClaudeExecutor(&config.Config{}).FetchClaudeQuota(ctx, a); errFetch == nil {
		t.Fatal("cancellation succeeded")
	}
}
