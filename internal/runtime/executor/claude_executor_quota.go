package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchClaudeQuota uses the same credential proxy and TLS transport as inference.
// The fixed endpoint and redirect guard keep OAuth tokens on Anthropic's origin.
func (e *ClaudeExecutor) FetchClaudeQuota(ctx context.Context, auth *cliproxyauth.Auth) (cliproxyauth.ClaudeQuotaSnapshot, error) {
	token, baseURL := claudeCreds(auth)
	if !isClaudeOAuthToken(token) || (baseURL != "" && baseURL != "https://api.anthropic.com") {
		return nil, &cliproxyauth.ClaudeQuotaProbeError{Status: http.StatusForbidden}
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	client := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, errResponse := client.Do(req)
	if errResponse != nil {
		return nil, fmt.Errorf("Claude quota transport failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("could not close Claude quota response")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		retry := time.Time{}
		if seconds, errSeconds := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); errSeconds == nil && seconds > 0 {
			retry = time.Now().Add(time.Duration(seconds) * time.Second)
		} else if date, errDate := http.ParseTime(resp.Header.Get("Retry-After")); errDate == nil {
			retry = date
		}
		return nil, &cliproxyauth.ClaudeQuotaProbeError{Status: resp.StatusCode, RetryAt: retry}
	}
	const maxUsageBody = 1024 * 1024
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	if errRead != nil || len(raw) > maxUsageBody {
		return nil, fmt.Errorf("could not read Claude quota response")
	}
	return cliproxyauth.ParseClaudeQuotaUsage(raw, time.Now())
}
