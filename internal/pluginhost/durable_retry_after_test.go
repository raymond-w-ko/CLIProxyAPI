package pluginhost

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestPluginRetryAfterPreservesDurableOwner(t *testing.T) {
	for _, source := range []string{"envelope", "stream detail"} {
		t.Run(source, func(t *testing.T) {
			detail := &pluginabi.Error{Code: "rate_limited", Message: "try later", HTTPStatus: http.StatusTooManyRequests, RetryAfterSeconds: 3600}
			var rateErr error
			if source == "envelope" {
				_, rateErr = decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{Error: detail})
			} else {
				rateErr = rpcStreamError("", detail)
			}
			const provider = "plugin-durable-retry"
			ids := []string{t.Name() + "-a", t.Name() + "-b"}
			fail := false
			var calls []string
			record := normalizeTestCapabilityRecord(capabilityRecord{id: "durable-retry-plugin"})
			host := newHostWithRecords(record)
			adapter := newExecutorAdapterForRecordForTest(host, record, &fakeExecutor{
				identifier: provider,
				execute: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
					calls = append(calls, req.AuthID)
					if fail && req.AuthID == ids[1] {
						return pluginapi.ExecutorResponse{}, rateErr
					}
					return pluginapi.ExecutorResponse{Payload: []byte(`{"id":"ok"}`)}, nil
				},
			}, []sdktranslator.Format{sdktranslator.FormatOpenAI}, []sdktranslator.Format{sdktranslator.FormatOpenAI})
			adapter.provider = provider
			path := filepath.Join(t.TempDir(), "bindings.json")
			newManager := func() *coreauth.Manager {
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{File: path, Fallback: &coreauth.FillFirstSelector{}})
				t.Cleanup(selector.Stop)
				manager := coreauth.NewManager(nil, selector, nil)
				manager.RegisterExecutor(adapter)
				for _, id := range ids {
					if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: "model-one"}, {ID: "model-two"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				return manager
			}
			manager := newManager()
			opts := coreexecutor.Options{Headers: http.Header{"X-Session-Id": {"plugin-session"}}, SourceFormat: sdktranslator.FormatOpenAI,
				Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: ids[1]}}
			req := coreexecutor.Request{Model: "model-one", Payload: []byte(`{"model":"model-one"}`)}
			if _, errExecute := manager.Execute(context.Background(), []string{provider}, req, opts); errExecute != nil {
				t.Fatal(errExecute)
			}
			delete(opts.Metadata, coreexecutor.PinnedAuthMetadataKey)
			fail = true
			if _, errExecute := manager.Execute(context.Background(), []string{provider}, req, opts); errExecute == nil {
				t.Fatal("generic retry-after error escaped the durable owner")
			}
			for _, id := range calls {
				if id != ids[1] {
					t.Fatalf("unconfirmed quota invoked another account: %q", id)
				}
			}
			fail = false
			restarted := newManager()
			req.Model, req.Payload = "model-two", []byte(`{"model":"model-two"}`)
			if _, errExecute := restarted.Execute(context.Background(), []string{provider}, req, opts); errExecute != nil {
				t.Fatal(errExecute)
			}
			if calls[len(calls)-1] != ids[1] {
				t.Fatal("restart lost the saved owner")
			}
		})
	}
}
