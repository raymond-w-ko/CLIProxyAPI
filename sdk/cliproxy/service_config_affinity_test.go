package cliproxy

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
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
