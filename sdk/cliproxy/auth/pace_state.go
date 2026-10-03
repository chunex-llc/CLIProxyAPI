package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// paceStateFileName must not end in .json (the auth loader parses every .json
// file as a credential) or .cds (the cooldown state loader).
const paceStateFileName = "pace.state"

const paceStateVersion = 1

// SessionBinding is one persisted session-affinity group. Aliases[0] is the
// group's primary key.
type SessionBinding struct {
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Aliases   []string  `json:"aliases"`
}

// SnapshotBindings returns every binding group still valid at now.
func (c *SessionCache) SnapshotBindings(now time.Time) []SessionBinding {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	bindings := make([]SessionBinding, 0, len(c.groups))
	for _, group := range c.groups {
		if !group.expiresAt.After(now) || len(group.aliases) == 0 {
			continue
		}
		bindings = append(bindings, SessionBinding{
			AuthID:    group.authID,
			ExpiresAt: group.expiresAt,
			Aliases:   append([]string(nil), group.aliases...),
		})
	}
	return bindings
}

// RestoreBindings installs bindings still valid at now, replacing any existing
// entries for the same keys.
func (c *SessionCache) RestoreBindings(bindings []SessionBinding, now time.Time) {
	if c == nil || len(bindings) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	for _, binding := range bindings {
		if binding.AuthID == "" || len(binding.Aliases) == 0 || !binding.ExpiresAt.After(now) {
			continue
		}
		var previous []sessionEntry
		for _, alias := range binding.Aliases {
			if entry, ok := c.entries[alias]; ok {
				previous = append(previous, entry)
			}
		}
		c.replaceAliasGroupsLocked(binding.AuthID, binding.ExpiresAt, append([]string(nil), binding.Aliases...), previous...)
	}
}

// SnapshotBindings returns the selector's session bindings still valid at now.
func (s *SessionAffinitySelector) SnapshotBindings(now time.Time) []SessionBinding {
	if s == nil {
		return nil
	}
	return s.cache.SnapshotBindings(now)
}

// RestoreBindings installs previously snapshotted session bindings.
func (s *SessionAffinitySelector) RestoreBindings(bindings []SessionBinding, now time.Time) {
	if s == nil {
		return
	}
	s.cache.RestoreBindings(bindings, now)
}

// PaceState is the pace ledger and session bindings saved across restarts.
type PaceState struct {
	Version  int                                          `json:"version"`
	SavedAt  time.Time                                    `json:"saved_at"`
	Windows  map[string]map[PaceWindowKey]PaceObservation `json:"windows"`
	Bindings []SessionBinding                             `json:"bindings"`
}

// PaceStateStore reads and writes <authDir>/pace.state.
type PaceStateStore struct {
	path string
}

// NewPaceStateStore returns a store for the pace state file in authDir.
func NewPaceStateStore(authDir string) *PaceStateStore {
	return &PaceStateStore{path: filepath.Join(authDir, paceStateFileName)}
}

// Save atomically replaces the state file with state.
func (s *PaceStateStore) Save(state PaceState) error {
	state.Version = paceStateVersion
	data, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		return fmt.Errorf("marshal pace state: %w", errMarshal)
	}
	dir := filepath.Dir(s.path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create pace state directory: %w", errMkdir)
	}
	tmpFile, errCreate := os.CreateTemp(dir, paceStateFileName+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create pace state temp file: %w", errCreate)
	}
	tmp := tmpFile.Name()
	fail := func(err error) error {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return err
	}
	if errChmod := tmpFile.Chmod(0o600); errChmod != nil {
		return fail(fmt.Errorf("chmod pace state temp file: %w", errChmod))
	}
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		return fail(fmt.Errorf("write pace state temp file: %w", errWrite))
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close pace state temp file: %w", errClose)
	}
	if errRename := os.Rename(tmp, s.path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace pace state file: %w", errRename)
	}
	return nil
}

// Load reads the state file. A missing file yields an empty state and no
// error; an unreadable or incompatible file yields an empty state and an error.
func (s *PaceStateStore) Load() (PaceState, error) {
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return PaceState{}, nil
		}
		return PaceState{}, fmt.Errorf("read pace state: %w", errRead)
	}
	var state PaceState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return PaceState{}, fmt.Errorf("parse pace state: %w", errUnmarshal)
	}
	if state.Version != paceStateVersion {
		return PaceState{}, fmt.Errorf("pace state version %d, want %d", state.Version, paceStateVersion)
	}
	return state, nil
}
