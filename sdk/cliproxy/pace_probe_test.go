package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type paceProbeFixture struct {
	probe   *paceProbe
	ledger  *coreauth.PaceLedger
	manager *coreauth.Manager
}

// newPaceProbeFixture registers one Claude auth "a" and one Codex auth "b" and
// points the probe's usage URLs at handlers.
func newPaceProbeFixture(t *testing.T, claude, codex http.HandlerFunc) paceProbeFixture {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/claude", claude)
	mux.HandleFunc("/codex", codex)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "tok-a"}},
		{ID: "b", Provider: "codex", Metadata: map[string]any{"access_token": "tok-b", "account_id": "acct-b"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	ledger := coreauth.NewPaceLedger()
	probe := newPaceProbe(&internalconfig.Config{}, manager, ledger, time.Minute)
	probe.claudeUsageURL = server.URL + "/claude"
	probe.codexUsageURL = server.URL + "/codex"
	return paceProbeFixture{probe: probe, ledger: ledger, manager: manager}
}

func paceStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func paceJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestPaceProbeRecordsClaudeUsage(t *testing.T) {
	fixture := newPaceProbeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-a" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", got)
		}
		paceJSON(`{"five_hour":{"utilization":10,"resets_at":"2026-10-03T12:00:00Z"},"seven_day":{"utilization":50,"resets_at":"2026-10-07T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":95,"resets_at":"2026-10-06T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`)(w, r)
	}, paceStatus(http.StatusServiceUnavailable))

	fixture.probe.cycle(context.Background())

	windows := fixture.ledger.Snapshot()["a"]
	if got := windows["5h"]; got.UsedPercent != 10 || got.Duration != 5*time.Hour {
		t.Fatalf("5h = %+v", got)
	}
	if got := windows["7d"]; got.UsedPercent != 50 || got.Duration != 168*time.Hour {
		t.Fatalf("7d = %+v", got)
	}
	wantReset := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if got := windows["model:fable"]; got.UsedPercent != 95 || !got.ResetAt.Equal(wantReset) {
		t.Fatalf("model:fable = %+v", got)
	}
}

func TestPaceProbeRecordsCodexUsage(t *testing.T) {
	fixture := newPaceProbeFixture(t, paceStatus(http.StatusServiceUnavailable), func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "acct-b" {
			t.Errorf("Chatgpt-Account-Id = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "codex-1" {
			t.Errorf("OpenAI-Beta = %q", got)
		}
		paceJSON(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":30,"reset_at":1790000000,"limit_window_seconds":18000},"secondary_window":{"used_percent":60,"reset_at":1790500000,"limit_window_seconds":604800}}}`)(w, r)
	})

	fixture.probe.cycle(context.Background())

	windows := fixture.ledger.Snapshot()["b"]
	if got := windows["primary"]; got.UsedPercent != 30 || got.Duration != 5*time.Hour || !got.ResetAt.Equal(time.Unix(1790000000, 0)) {
		t.Fatalf("primary = %+v", got)
	}
	if got := windows["secondary"]; got.UsedPercent != 60 {
		t.Fatalf("secondary = %+v", got)
	}
}

func TestPaceProbeRejectedLeavesLedgerAndAuthUnchanged(t *testing.T) {
	fixture := newPaceProbeFixture(t, paceStatus(http.StatusUnauthorized), paceStatus(http.StatusUnauthorized))
	before := map[string]*coreauth.Auth{}
	for _, id := range []string{"a", "b"} {
		auth, _ := fixture.manager.GetByID(id)
		before[id] = auth
	}

	fixture.probe.cycle(context.Background())

	if snapshot := fixture.ledger.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("ledger = %+v, want empty", snapshot)
	}
	for id, was := range before {
		now, _ := fixture.manager.GetByID(id)
		if now.Unavailable != was.Unavailable || now.Status != was.Status || !reflect.DeepEqual(now.Quota, was.Quota) {
			t.Fatalf("auth %s changed: before %+v, after %+v", id, was, now)
		}
	}
}

func TestPaceParseCodexUsageNullRateLimit(t *testing.T) {
	obs, err := parseCodexUsage([]byte(`{"rate_limit":null}`), time.Now())
	if err != nil || len(obs) != 0 {
		t.Fatalf("obs = %+v, err = %v; want empty, nil", obs, err)
	}
}

func TestPaceParseCodexUsageTopLevelAdditionalLimits(t *testing.T) {
	obs, err := parseCodexUsage([]byte(`{"additional_rate_limits":[{"limit_name":"GPT-5.3 Codex Spark","rate_limit":{"primary_window":{"used_percent":80,"reset_at":1790000000,"limit_window_seconds":604800}}}]}`), time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, ok := obs["model:gpt-5.3 codex spark"]; !ok || got.UsedPercent != 80 {
		t.Fatalf("obs = %+v", obs)
	}
}

func TestPaceProbeAfterCycleOncePerCycle(t *testing.T) {
	fixture := newPaceProbeFixture(t, paceJSON(`{}`), paceJSON(`{}`))
	calls := 0
	fixture.probe.afterCycle = func() { calls++ }

	for cycle := 1; cycle <= 3; cycle++ {
		fixture.probe.cycle(context.Background())
		if calls != cycle {
			t.Fatalf("after %d cycles afterCycle ran %d times", cycle, calls)
		}
	}
}

func TestPaceProbeRunDisabledReturnsImmediately(t *testing.T) {
	fixture := newPaceProbeFixture(t, paceJSON(`{}`), paceJSON(`{}`))
	fixture.probe.interval = 0
	cycled := false
	fixture.probe.afterCycle = func() { cycled = true }

	done := make(chan struct{})
	go func() {
		fixture.probe.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with interval 0 did not return")
	}
	if cycled {
		t.Fatal("Run with interval 0 probed")
	}
}

func TestPaceProbeIntervalConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"15m", 15 * time.Minute},
		{"bogus", 10 * time.Minute},
		{"", 10 * time.Minute},
	} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: "pace", PaceProbeInterval: tc.value},
		})
		if state.paceProbeInterval != tc.want {
			t.Errorf("pace-probe-interval %q = %s, want %s", tc.value, state.paceProbeInterval, tc.want)
		}
	}
}
