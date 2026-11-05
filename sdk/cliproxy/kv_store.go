package cliproxy

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func (s *Service) applyKVStoreConfig(cfg *config.Config) error {
	if s != nil {
		s.kvStoreManaged.Store(true)
	}
	if s == nil || cfg == nil || cfg.Home.Enabled || home.Current() != nil || cfg.KVStore.Disabled {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			return fmt.Errorf("shutdown local kv store: %w", errShutdown)
		}
		return nil
	}
	if errConfigure := kvstore.ConfigureLocal(true, cfg.EffectiveKVStorePath()); errConfigure != nil {
		return fmt.Errorf("configure local kv store: %w", errConfigure)
	}
	return nil
}

func (s *Service) shutdownKVStore() error {
	if s == nil {
		return nil
	}
	s.kvStoreManaged.Store(false)
	return kvstore.ShutdownLocal()
}
