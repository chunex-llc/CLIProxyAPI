package cliproxy

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// paceEnabled reports whether the configured routing strategy is pace.
func (s *Service) paceEnabled() bool {
	return normalizedRoutingRuntimeState(s.cfg).strategy == "pace"
}

// paceStateStore returns the store for <auth-dir>/pace.state, or nil when the
// auth directory cannot be resolved.
func (s *Service) paceStateStore() *coreauth.PaceStateStore {
	if s.cfg == nil {
		return nil
	}
	authDir, errResolve := util.ResolveAuthDir(s.cfg.AuthDir)
	if errResolve != nil {
		log.Warnf("failed to resolve pace state directory: %v", errResolve)
		return nil
	}
	if authDir == "" {
		log.Warn("pace state not persisted: auth directory is empty")
		return nil
	}
	return coreauth.NewPaceStateStore(authDir)
}

// loadPaceState restores the pace ledger and session bindings saved by the
// previous run. Windows of auths that no longer exist are dropped.
func (s *Service) loadPaceState(ctx context.Context) {
	store := s.paceStateStore()
	if store == nil || s.coreManager == nil {
		return
	}
	state, errLoad := store.Load()
	if errLoad != nil {
		log.Warnf("failed to load pace state: %v", errLoad)
	}
	coreauth.DefaultPaceLedger().Restore(state.Windows, func(authID string) bool {
		_, ok := s.coreManager.GetByID(authID)
		return ok
	})
	if selector, ok := s.coreManager.Selector().(*coreauth.SessionAffinitySelector); ok {
		selector.RestoreBindings(state.Bindings, time.Now())
	}
}

// savePaceState writes the pace ledger and session bindings to
// <auth-dir>/pace.state. It is safe to call repeatedly.
func (s *Service) savePaceState() {
	if s == nil || s.coreManager == nil || !s.paceEnabled() {
		return
	}
	store := s.paceStateStore()
	if store == nil {
		return
	}
	now := time.Now()
	state := coreauth.PaceState{
		SavedAt: now,
		Windows: coreauth.DefaultPaceLedger().Snapshot(),
	}
	if selector, ok := s.coreManager.Selector().(*coreauth.SessionAffinitySelector); ok {
		state.Bindings = selector.SnapshotBindings(now)
	}
	if errSave := store.Save(state); errSave != nil {
		log.Warnf("failed to save pace state: %v", errSave)
		return
	}
	log.Infof("pace state saved (auths=%d, bindings=%d)", len(state.Windows), len(state.Bindings))
}
