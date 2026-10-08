package cliproxy

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestRoutingDurableAffinityUsesConfigDirectory(t *testing.T) {
	var cfg config.Config
	if errDecode := yaml.Unmarshal([]byte("routing:\n  session-affinity: true\n"), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, path := range []string{"config.yaml", filepath.Join("relative", "custom.yaml"), filepath.Join(t.TempDir(), "custom.yaml")} {
		state := normalizedRoutingRuntimeState(&cfg, path)
		want := filepath.Join(filepath.Dir(path), "session-bindings.json")
		if !state.sessionAffinity || state.sessionAffinityFile != want {
			t.Fatalf("routing for %q = %+v, want %q", path, state, want)
		}
		selector := newRoutingSelector(state)
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("selector = %T", selector)
		}
		affinity.Stop()
	}
	cfg.Routing.SessionAffinity = false
	state := normalizedRoutingRuntimeState(&cfg, "config.yaml")
	if state.sessionAffinity || state.sessionAffinityFile != "" {
		t.Fatalf("disabled affinity retained durable path: %+v", state)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.RoundRobinSelector); !ok {
		t.Fatal("disabled affinity must restore ordinary routing")
	}
}

func TestServiceDurableAffinityToggleAndRestart(t *testing.T) {
	ctx := coreauth.WithSkipPersist(context.Background())
	configPath := filepath.Join(t.TempDir(), "configuration", "custom.yaml")
	bindingsPath := filepath.Join(filepath.Dir(configPath), "session-bindings.json")
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.Strategy = "fill-first"
	cfg.Routing.SessionAffinity = true
	newService := func() *Service {
		t.Helper()
		s, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		t.Cleanup(func() {
			if selector, ok := s.coreManager.Selector().(coreauth.StoppableSelector); ok {
				selector.Stop()
			}
		})
		s.coreManager.RegisterExecutor(serviceTestPluginExecutor{})
		for _, id := range []string{"durable-a", "durable-b"} {
			if _, errRegister := s.coreManager.Register(ctx, &coreauth.Auth{ID: id, Provider: "plugin-provider", Status: coreauth.StatusActive}); errRegister != nil {
				t.Fatal(errRegister)
			}
		}
		return s
	}
	service := newService()
	selectOwner := func(s *Service, opts cliproxyexecutor.Options, want string) {
		t.Helper()
		a, errSelect := s.coreManager.SelectAuth(ctx, "plugin-provider", "", opts)
		if errSelect != nil || a.ID != want {
			t.Fatalf("selection = %v, %v; want %s", a, errSelect, want)
		}
	}
	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Session-Id": {"persisted-session"}},
		Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "durable-b"},
	}
	selectOwner(service, opts, "durable-b")
	before, errRead := os.ReadFile(bindingsPath)
	if errRead != nil {
		t.Fatalf("binding missing beside config: %v", errRead)
	}
	delete(opts.Metadata, cliproxyexecutor.PinnedAuthMetadataKey)
	disabled := *cfg
	disabled.Routing.SessionAffinity = false
	if !service.applyManagerConfig(ctx, configCommit{cfg: &disabled}) {
		t.Fatal("disable affinity failed")
	}
	selectOwner(service, cliproxyexecutor.Options{}, "durable-a")
	after, errRead := os.ReadFile(bindingsPath)
	if errRead != nil || !bytes.Equal(before, after) {
		t.Fatalf("disabled routing changed saved bindings: %v", errRead)
	}
	if !service.applyManagerConfig(ctx, configCommit{cfg: cfg}) {
		t.Fatal("enable affinity failed")
	}
	selectOwner(service, opts, "durable-b")
	selectOwner(newService(), opts, "durable-b")
}

func TestServiceDurableAffinityBatchModelExclusionKeepsOwner(t *testing.T) {
	ctx := coreauth.WithSkipPersist(context.Background())
	path := filepath.Join(t.TempDir(), "session-bindings.json")
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		File: path, Fallback: &coreauth.FillFirstSelector{},
	})
	t.Cleanup(selector.Stop)
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(mockBatchTestExecutor{provider: "codex"})
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for _, id := range ids {
		if _, errRegister := manager.Register(ctx, &coreauth.Auth{
			ID: id, Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
	}
	service.registerModelsForAuthBatch(ctx, manager.List())
	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Session-Id": {"batch-session"}},
		Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: ids[1]},
	}
	if owner, errSelect := manager.SelectAuth(ctx, "codex", "gpt-6-astra", opts); errSelect != nil || owner.ID != ids[1] {
		t.Fatalf("initial owner = %v, %v", owner, errSelect)
	}
	delete(opts.Metadata, cliproxyexecutor.PinnedAuthMetadataKey)
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var changed atomic.Bool
	modelRegistrationTaskPostRunHook = func(id string) {
		if id != ids[1] || !changed.CompareAndSwap(false, true) {
			return
		}
		current, _ := manager.GetByID(id)
		current.Attributes["excluded_models"] = "gpt-6-astra"
		if _, errUpdate := manager.Update(ctx, current); errUpdate != nil {
			t.Errorf("exclude owner model: %v", errUpdate)
		}
	}
	t.Cleanup(func() { modelRegistrationTaskPostRunHook = nil })
	service.registerModelsForAuthBatch(ctx, manager.List())
	if !changed.Load() || GlobalModelRegistry().ClientSupportsModel(ids[1], "gpt-6-astra") {
		t.Fatal("batch did not apply the concurrent model exclusion")
	}
	if !GlobalModelRegistry().ClientSupportsModel(ids[0], "gpt-6-astra") {
		t.Fatal("backup must still support the excluded model")
	}
	if owner, errSelect := manager.SelectAuth(ctx, "codex", "gpt-6-astra", opts); errSelect == nil || owner != nil {
		t.Fatalf("model exclusion must fail without migrating: %v, %v", owner, errSelect)
	}
	if owner, errSelect := manager.SelectAuth(ctx, "codex", "gpt-6.1-sol", opts); errSelect != nil || owner.ID != ids[1] {
		t.Fatalf("batch changed ownership for a supported model: %v, %v", owner, errSelect)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil || !bytes.Equal(before, after) {
		t.Fatalf("batch reconciliation changed persisted bindings: %v", errRead)
	}
}
