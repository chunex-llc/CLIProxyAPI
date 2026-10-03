package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// These tests share the process-wide pace ledger, so they use unique auth IDs
// and do not run in parallel.

func paceRuntimeService(t *testing.T, strategy string) *Service {
	t.Helper()
	selector := coreauth.NewSessionAffinitySelector(&coreauth.PaceSelector{})
	t.Cleanup(selector.Stop)
	return &Service{
		cfg: &internalconfig.Config{
			AuthDir: t.TempDir(),
			Routing: internalconfig.RoutingConfig{Strategy: strategy, SessionAffinity: true},
		},
		coreManager: coreauth.NewManager(nil, selector, nil),
	}
}

func TestPaceRuntime_StateSurvivesRestart(t *testing.T) {
	service := paceRuntimeService(t, "pace")
	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{ID: "pace-runtime-a", Provider: "codex"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	ledger := coreauth.DefaultPaceLedger()
	t.Cleanup(func() { ledger.Restore(nil, nil) })
	now := time.Now()
	ledger.Observe("pace-runtime-a", map[coreauth.PaceWindowKey]coreauth.PaceObservation{"5h": {UsedPercent: 30, ObservedAt: now}})
	ledger.Observe("pace-runtime-x", map[coreauth.PaceWindowKey]coreauth.PaceObservation{"5h": {UsedPercent: 60, ObservedAt: now}})

	service.savePaceState()
	ledger.Restore(nil, nil)
	service.loadPaceState(context.Background())

	snapshot := ledger.Snapshot()
	if got := snapshot["pace-runtime-a"]["5h"].UsedPercent; got != 30 {
		t.Fatalf("restored windows for a = %+v, want 5h at 30%%", snapshot["pace-runtime-a"])
	}
	if got := snapshot["pace-runtime-x"]; len(got) != 0 {
		t.Fatalf("windows for removed auth x restored: %+v", got)
	}
}

func TestPaceRuntime_SaveSkippedWithoutPace(t *testing.T) {
	service := paceRuntimeService(t, "round-robin")
	service.savePaceState()
	if _, err := os.Stat(filepath.Join(service.cfg.AuthDir, "pace.state")); !os.IsNotExist(err) {
		t.Fatalf("pace.state stat error = %v, want not exist", err)
	}
}
