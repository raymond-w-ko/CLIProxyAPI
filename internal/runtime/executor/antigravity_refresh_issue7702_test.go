package executor

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestIssue7702AntigravityLaterRefreshReplacesStalledCreditsLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := issue6199AntigravityAuth(t.Name(), t.Name()+"-refresh")
		issue6199CreditsLifecycleFixture(t, auth.ID)

		var creditsRequest context.Context
		creditsCalls := 0
		var stalled atomic.Bool
		stalled.Store(true)
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://oauth2.googleapis.com/token":
				return issue6199AntigravityJSONResponse(req, `{"access_token":"new-access","expires_in":3600}`), nil
			case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
				creditsCalls++
				if !stalled.Load() {
					return issue6199CreditsBalanceResponse(req, 900), nil
				}
				creditsRequest = req.Context()
				// Emulate an upstream that accepts the connection but never
				// sends headers: block until the request context is done.
				<-req.Context().Done()
				return nil, req.Context().Err()
			default:
				return nil, fmt.Errorf("unexpected intercepted request: %s", req.URL)
			}
		})
		lifecycle, cancelLifecycle := context.WithCancel(t.Context())
		defer cancelLifecycle()
		ctx := context.WithValue(lifecycle, "cliproxy.roundtripper", transport)
		executor := newIssue6199AntigravityCreditsExecutor()

		updated := issue6199RefreshForCreditsLifecycle(t, executor, ctx, auth)
		if updated == nil {
			t.Fatal("token refresh did not complete")
		}
		if creditsCalls != 1 {
			t.Fatalf("post-refresh credits calls = %d, want 1", creditsCalls)
		}
		if creditsRequest == nil {
			t.Fatal("post-refresh credits request did not start")
		}
		if _, hasDeadline := creditsRequest.Deadline(); hasDeadline {
			t.Error("post-refresh credits request acquired a network deadline")
		}

		// The next refresh can replace a stalled lookup after the retry interval.
		timer := time.NewTimer(antigravityCreditsHintRefreshInterval)
		defer timer.Stop()
		<-timer.C
		stalled.Store(false)
		issue6199RefreshForCreditsLifecycle(t, executor, ctx, updated)
		synctest.Wait()
		if creditsRequest.Err() != context.Canceled || creditsCalls != 2 {
			t.Fatalf("stalled request error = %v, credits calls = %d, want cancellation and 2 calls", creditsRequest.Err(), creditsCalls)
		}
		issue6199AssertCreditsBalance(t, updated, 900)
	})
}
