package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func paceStateSelector(t *testing.T, ledger *PaceLedger) *SessionAffinitySelector {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &PaceSelector{Ledger: ledger}, TTL: time.Hour})
	t.Cleanup(selector.Stop)
	return selector
}

// paceStateLedger makes winner far behind pace and loser far ahead.
func paceStateLedger(winner, loser string) *PaceLedger {
	ledger := NewPaceLedger()
	now := time.Now()
	week := 10080 * time.Minute
	observeCodexPace(ledger, winner, 20, week, now.Add(24*time.Hour))
	observeCodexPace(ledger, loser, 70, week, now.Add(48*time.Hour))
	return ledger
}

func paceStateSessionOpts(sessionID string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:  map[string][]string{"X-Claude-Code-Session-Id": {sessionID}},
		Metadata: make(map[string]any),
	}
}

func paceStateAuths() []*Auth {
	return []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
}

func TestPaceState_BindingsSurviveRestore(t *testing.T) {
	t.Parallel()
	before := paceStateSelector(t, paceStateLedger("a", "b"))
	if got := pickPace(t, before, paceStateSessionOpts("s1"), paceStateAuths()); got != "a" {
		t.Fatalf("first Pick() = %q, want a", got)
	}

	before.cache.mu.RLock()
	var cacheKey string
	for key := range before.cache.groups {
		cacheKey = key
	}
	groupCount := len(before.cache.groups)
	before.cache.mu.RUnlock()
	if groupCount != 1 || !strings.HasPrefix(cacheKey, "codex::") || !strings.Contains(cacheKey, "s1") {
		t.Fatalf("cache groups = %d (key %q), want one codex::…s1… group", groupCount, cacheKey)
	}

	bindings := before.SnapshotBindings(time.Now())
	if len(bindings) != 1 || bindings[0].AuthID != "a" || len(bindings[0].Aliases) == 0 || bindings[0].Aliases[0] != cacheKey {
		t.Fatalf("SnapshotBindings() = %+v, want one binding to a keyed %q", bindings, cacheKey)
	}

	after := paceStateSelector(t, paceStateLedger("b", "a"))
	after.RestoreBindings(bindings, time.Now())
	if got := pickPace(t, after, paceStateSessionOpts("s1"), paceStateAuths()); got != "a" {
		t.Fatalf("restored session Pick() = %q, want a", got)
	}
	if got := pickPace(t, after, paceStateSessionOpts("s2"), paceStateAuths()); got != "b" {
		t.Fatalf("new session Pick() = %q, want b", got)
	}
}

func TestPaceState_ExpiredBindingNotRestored(t *testing.T) {
	t.Parallel()
	before := paceStateSelector(t, paceStateLedger("a", "b"))
	pickPace(t, before, paceStateSessionOpts("s1"), paceStateAuths())
	bindings := before.SnapshotBindings(time.Now())
	if len(bindings) != 1 {
		t.Fatalf("SnapshotBindings() = %+v, want one binding", bindings)
	}
	bindings[0].ExpiresAt = time.Now().Add(-time.Minute)

	after := paceStateSelector(t, paceStateLedger("b", "a"))
	after.RestoreBindings(bindings, time.Now())
	if got := after.SnapshotBindings(time.Now()); len(got) != 0 {
		t.Fatalf("expired binding restored: %+v", got)
	}
	if got := pickPace(t, after, paceStateSessionOpts("s1"), paceStateAuths()); got != "b" {
		t.Fatalf("Pick() = %q, want the fallback's choice b", got)
	}
}

func TestPaceStateStore_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewPaceStateStore(dir)
	observed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.FixedZone("x", 3600))
	state := PaceState{
		SavedAt: observed,
		Windows: map[string]map[PaceWindowKey]PaceObservation{
			"a": {
				"5h":          {UsedPercent: 42.5, ResetAt: observed.Add(time.Hour), Duration: 5 * time.Hour, ObservedAt: observed},
				"model:fable": {UsedPercent: 10, ObservedAt: observed},
			},
		},
		Bindings: []SessionBinding{{AuthID: "a", ExpiresAt: observed.Add(time.Hour), Aliases: []string{"codex::s1::m", "pck:x"}}},
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "pace.state"))
	if err != nil {
		t.Fatalf("stat pace.state: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("pace.state mode = %o, want 600", mode)
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmps) != 0 {
		t.Fatalf("temp files remain: %v", tmps)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Version != paceStateVersion {
		t.Fatalf("Version = %d, want %d", loaded.Version, paceStateVersion)
	}
	if !reflect.DeepEqual(normalizePaceWindows(loaded.Windows), normalizePaceWindows(state.Windows)) {
		t.Fatalf("Windows = %+v, want %+v", loaded.Windows, state.Windows)
	}
	if !reflect.DeepEqual(normalizeBindings(loaded.Bindings), normalizeBindings(state.Bindings)) {
		t.Fatalf("Bindings = %+v, want %+v", loaded.Bindings, state.Bindings)
	}
}

func TestPaceStateStore_LoadMissingFile(t *testing.T) {
	t.Parallel()
	state, err := NewPaceStateStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(state, PaceState{}) {
		t.Fatalf("Load() = %+v, want empty", state)
	}
}

func TestPaceStateStore_LoadWrongVersion(t *testing.T) {
	t.Parallel()
	assertPaceStateLoadFails(t, `{"version":99}`)
}

func TestPaceStateStore_LoadTruncated(t *testing.T) {
	t.Parallel()
	assertPaceStateLoadFails(t, `{"version":1,"windows":{"a":{"5h":{"used_perc`)
}

func assertPaceStateLoadFails(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pace.state"), []byte(content), 0o600); err != nil {
		t.Fatalf("write pace.state: %v", err)
	}
	state, err := NewPaceStateStore(dir).Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if !reflect.DeepEqual(state, PaceState{}) {
		t.Fatalf("Load() = %+v, want empty", state)
	}
}

func normalizePaceWindows(windows map[string]map[PaceWindowKey]PaceObservation) map[string]map[PaceWindowKey]PaceObservation {
	out := make(map[string]map[PaceWindowKey]PaceObservation, len(windows))
	for authID, current := range windows {
		out[authID] = make(map[PaceWindowKey]PaceObservation, len(current))
		for key, observation := range current {
			observation.ResetAt = observation.ResetAt.UTC()
			observation.ObservedAt = observation.ObservedAt.UTC()
			out[authID][key] = observation
		}
	}
	return out
}

func normalizeBindings(bindings []SessionBinding) []SessionBinding {
	out := make([]SessionBinding, len(bindings))
	for index, binding := range bindings {
		binding.ExpiresAt = binding.ExpiresAt.UTC()
		out[index] = binding
	}
	return out
}
