package cliproxy

import (
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"testing"
)

func TestRoutingSoonestResetFirst(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Strategy = "soonest-reset-first"
	state := normalizedRoutingRuntimeState(cfg, "config.yaml")
	if state.strategy != "soonest-reset-first" {
		t.Fatal(state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetFirstSelector); !ok {
		t.Fatal("wrong selector")
	}
	cfg.Routing.SessionAffinity = true
	selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg, "config.yaml"))
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatal("missing affinity wrapper")
	}
	affinity.Stop()
}
