package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

// Fluid writes its managed hub config as JSON, which the YAML loader accepts.
func TestPaceRuntime_ReservationFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `{"routing":{"strategy":"pace","session-affinity":true,` +
		`"reservation":{"enabled":true,"reserve-percent":8,"release-before-reset-minutes":120}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := internalconfig.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
	if stoppable, ok := selector.(interface{ Stop() }); ok {
		t.Cleanup(stoppable.Stop)
	}

	ledger := coreauth.DefaultPaceLedger()
	t.Cleanup(func() { ledger.Restore(nil, nil) })
	now := time.Now()
	week := 7 * 24 * time.Hour
	// Unreserved, pace prefers a: its 8% left expires in three hours.
	ledger.Observe("pace-reserve-a", map[coreauth.PaceWindowKey]coreauth.PaceObservation{
		"secondary": {UsedPercent: 92, ResetAt: now.Add(3 * time.Hour), Duration: week, ObservedAt: now},
	})
	ledger.Observe("pace-reserve-b", map[coreauth.PaceWindowKey]coreauth.PaceObservation{
		"secondary": {UsedPercent: 20, ResetAt: now.Add(6 * 24 * time.Hour), Duration: week, ObservedAt: now},
	})
	auths := []*coreauth.Auth{{ID: "pace-reserve-a", Provider: "codex"}, {ID: "pace-reserve-b", Provider: "codex"}}
	got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "pace-reserve-b" {
		t.Fatalf("Pick() auth = %v, want pace-reserve-b", got)
	}
}

func TestPaceRuntime_UnchangedReservationReloadKeepsBinding(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)}
	reload := func() {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{
			Strategy:        "pace",
			SessionAffinity: true,
			Reservation:     internalconfig.PaceReservationConfig{Enabled: true, ReservePercent: 8, ReleaseBeforeResetMinutes: 120},
		}}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
			t.Fatal("applyManagerConfig failed")
		}
		if stoppable, ok := service.coreManager.Selector().(interface{ Stop() }); ok {
			t.Cleanup(stoppable.Stop)
		}
	}
	ledger := coreauth.DefaultPaceLedger()
	t.Cleanup(func() { ledger.Restore(nil, nil) })
	now := time.Now()
	week := 7 * 24 * time.Hour
	observe := func(id string, used float64) {
		ledger.Observe(id, map[coreauth.PaceWindowKey]coreauth.PaceObservation{
			"secondary": {UsedPercent: used, ResetAt: now.Add(3 * 24 * time.Hour), Duration: week, ObservedAt: time.Now()},
		})
	}
	auths := []*coreauth.Auth{{ID: "pace-reload-a", Provider: "codex"}, {ID: "pace-reload-b", Provider: "codex"}}
	session := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_33333333-3333-3333-3333-333333333333"}}`)}
	pick := func() string {
		t.Helper()
		got, err := service.coreManager.Selector().Pick(context.Background(), "codex", "", session, auths)
		if err != nil || got == nil {
			t.Fatalf("Pick() = %v, %v", got, err)
		}
		return got.ID
	}

	reload()
	observe("pace-reload-a", 20)
	observe("pace-reload-b", 70)
	if got := pick(); got != "pace-reload-a" {
		t.Fatalf("first Pick() = %q, want pace-reload-a", got)
	}
	// A new session would now go to b; the bound one must survive the reload.
	observe("pace-reload-a", 70)
	observe("pace-reload-b", 0)
	reload()
	if got := pick(); got != "pace-reload-a" {
		t.Fatalf("Pick() after an unchanged reload = %q, want the bound pace-reload-a", got)
	}
}
