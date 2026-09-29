package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	log "github.com/sirupsen/logrus"
)

// durableSessionStore is owned by one Manager. The mutex covers selection and
// persistence, never an upstream request. Separate processes must not share it.
type durableSessionStore struct {
	mu       sync.Mutex
	path     string
	bindings map[string]string
	aliases  map[string]string
	err      error
}

type durableSessionFile struct {
	Version  int               `json:"version"`
	Bindings map[string]string `json:"bindings"`
	Aliases  map[string]string `json:"aliases,omitempty"`
}

func loadDurableSessions(path string) *durableSessionStore {
	s := &durableSessionStore{path: path, bindings: make(map[string]string), aliases: make(map[string]string)}
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return s
	}
	if errRead != nil {
		s.err = fmt.Errorf("read session bindings: %w", errRead)
		return s
	}
	var file durableSessionFile
	if errDecode := json.Unmarshal(data, &file); errDecode != nil {
		s.err = fmt.Errorf("decode session bindings: %w", errDecode)
		return s
	}
	if file.Version != 1 || file.Bindings == nil {
		s.err = errors.New("unsupported or invalid session bindings file")
		return s
	}
	for key, owner := range file.Bindings {
		if key == "" || owner == "" {
			s.err = errors.New("empty session binding key or account")
			return s
		}
	}
	for alias, root := range file.Aliases {
		if alias == "" || file.Bindings[root] == "" || file.Bindings[alias] != "" {
			s.err = errors.New("invalid session binding alias")
			return s
		}
	}
	s.bindings = file.Bindings
	if file.Aliases != nil {
		s.aliases = file.Aliases
	}
	return s
}

func (s *durableSessionStore) root(key string) string {
	if root := s.aliases[key]; root != "" {
		return root
	}
	return key
}

func (s *durableSessionStore) owner(key string) string {
	return s.bindings[s.root(key)]
}

// bind writes before dispatch. A failed write leaves the old owner intact.
// Callers must hold mu.
func (s *durableSessionStore) bind(key, owner string, aliases ...string) error {
	keys := append([]string{key}, aliases...)
	root := s.root(key)
	for _, candidate := range keys {
		if s.owner(candidate) != "" {
			root = s.root(candidate)
			break
		}
	}
	unchanged := s.bindings[root] == owner
	for _, candidate := range keys {
		unchanged = unchanged && s.root(candidate) == root
	}
	if unchanged {
		return nil
	}
	previousBindings, previousAliases := maps.Clone(s.bindings), maps.Clone(s.aliases)
	for _, candidate := range keys {
		otherRoot := s.root(candidate)
		if otherRoot == root {
			continue
		}
		// Merge known aliases as a group so later migrations cannot leave a
		// conversation ID pointing at an account its prompt cache key abandoned.
		for alias, target := range s.aliases {
			if target == otherRoot {
				s.aliases[alias] = root
			}
		}
		delete(s.bindings, otherRoot)
		s.aliases[otherRoot] = root
	}
	s.bindings[root] = owner
	data, errMarshal := json.Marshal(durableSessionFile{Version: 1, Bindings: s.bindings, Aliases: s.aliases})
	if errMarshal == nil {
		errMarshal = writeDurableSessions(s.path, data)
	}
	if errMarshal != nil {
		s.bindings, s.aliases = previousBindings, previousAliases
		// A rename may have succeeded before a directory sync failed. Stop
		// routing until restart rather than use potentially divergent state.
		s.err = fmt.Errorf("persist session bindings: %w", errMarshal)
	}
	return s.err
}

func writeDurableSessions(path string, data []byte) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	f, errCreate := os.CreateTemp(dir, ".session-bindings-*")
	if errCreate != nil {
		return errCreate
	}
	closed := false
	defer func() {
		if !closed {
			if errClose := f.Close(); errClose != nil {
				log.WithError(errClose).Error("close session bindings temporary file")
			}
		}
		if errRemove := os.Remove(f.Name()); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Error("remove session bindings temporary file")
		}
	}()
	if _, errWrite := f.Write(data); errWrite != nil {
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		return errSync
	}
	errClose := f.Close()
	closed = true
	if errClose != nil {
		return errClose
	}
	if errRename := os.Rename(f.Name(), path); errRename != nil {
		return errRename
	}
	if runtime.GOOS == "windows" {
		return nil // Windows does not support syncing directory handles this way.
	}
	d, errOpen := os.Open(dir)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := d.Close(); errClose != nil {
			log.WithError(errClose).Error("close session bindings directory")
		}
	}()
	return d.Sync()
}
