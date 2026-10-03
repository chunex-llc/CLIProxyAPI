package cliproxy

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const defaultPaceProbeInterval = 10 * time.Minute

// currentConfig reads the active config under the config lock, as reloads
// replace it.
func (s *Service) currentConfig() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// paceEnabled reports whether the configured routing strategy is pace.
func (s *Service) paceEnabled() bool {
	return normalizedRoutingRuntimeState(s.currentConfig()).strategy == "pace"
}

// paceProbeInterval parses routing.pace-probe-interval: "0" disables polling,
// a positive duration is used as is, anything else is the 10-minute default.
// It is kept out of routingRuntimeState on purpose: a change to that state
// rebuilds the selector and drops every session binding.
func paceProbeInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultPaceProbeInterval
	}
	interval := strings.TrimSpace(cfg.Routing.PaceProbeInterval)
	if interval == "0" {
		return 0
	}
	if parsed, errParse := time.ParseDuration(interval); errParse == nil && parsed > 0 {
		return parsed
	}
	return defaultPaceProbeInterval
}

// paceStateStore returns the store for <auth-dir>/pace.state, or nil when the
// auth directory cannot be resolved.
func (s *Service) paceStateStore() *coreauth.PaceStateStore {
	cfg := s.currentConfig()
	if cfg == nil {
		return nil
	}
	authDir, errResolve := util.ResolveAuthDir(cfg.AuthDir)
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
