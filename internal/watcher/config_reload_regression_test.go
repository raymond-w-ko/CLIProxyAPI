package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigWriteDuringReloadRemainsPending(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig := func(port int) {
		t.Helper()
		body := fmt.Sprintf("port: %d\nauth-dir: %q\n", port, filepath.ToSlash(authDir))
		if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	writeConfig(8317)
	var ports []int
	w := &Watcher{
		configPath: path,
		authDir:    authDir,
		reloadCallback: func(cfg *config.Config) {
			ports = append(ports, cfg.Port)
			if len(ports) == 1 {
				writeConfig(8318)
			}
		},
	}
	w.SetConfig(&config.Config{AuthDir: authDir})
	w.ReloadConfigIfChanged()
	w.ReloadConfigIfChanged()
	w.ReloadConfigIfChanged()
	if !reflect.DeepEqual(ports, []int{8317, 8318}) {
		t.Fatalf("loaded ports = %v; newer config must load exactly once", ports)
	}
}
